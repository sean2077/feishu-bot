package feishu

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

// Feishu deduplicates supported message requests for one hour. A UUID reduces
// duplicates; it is not evidence that an unacknowledged request failed. The
// journal therefore never replays an uncertain step without explicit recovery.
const messageDedupWindow = time.Hour

type deliveryStep struct {
	ID, Step, Room, Fingerprint, Payload, UUID, State, Result, Message string
	FirstAttempt, UpdatedAt, RetryAt int64
	Status, Code int
	RequestID string
}

func (c *Client) initDeliveryState() error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS feishu_deliveries (
			id TEXT NOT NULL,step TEXT NOT NULL,room TEXT NOT NULL,fingerprint TEXT NOT NULL,
			payload TEXT NOT NULL,uuid TEXT NOT NULL,state TEXT NOT NULL,result TEXT NOT NULL DEFAULT '',
			first_attempt INTEGER NOT NULL DEFAULT 0,updated_at INTEGER NOT NULL,message TEXT NOT NULL DEFAULT '',
			status INTEGER NOT NULL DEFAULT 0,code INTEGER NOT NULL DEFAULT 0,request_id TEXT NOT NULL DEFAULT '',
			retry_at INTEGER NOT NULL DEFAULT 0,attempt TEXT NOT NULL DEFAULT '',PRIMARY KEY(id,step))`,
		`CREATE INDEX IF NOT EXISTS feishu_deliveries_message ON feishu_deliveries(message)`,
		`CREATE TABLE IF NOT EXISTS feishu_card_lifecycle (
			message TEXT PRIMARY KEY,updated_at INTEGER NOT NULL,terminal_at INTEGER NOT NULL DEFAULT 0,
			stream_closed INTEGER NOT NULL DEFAULT 0)`,
	} {
		if _, err := c.db.Exec(statement); err != nil {
			return errors.New("feishu: delivery-state initialization failed")
		}
	}
	return nil
}

func (c *Client) deliveryContext(ctx context.Context) context.Context {
	if c.db != nil && core.DeliveryID(ctx) == "" {
		return core.WithDeliveryID(ctx, "transport:"+core.NewID())
	}
	return ctx
}

func validDeliveryID(id string) bool {
	return id != "" && len(id) <= 256 && !strings.ContainsAny(id, "\x00\r\n\t")
}

func (c *Client) loadDelivery(ctx context.Context, id, step string) (deliveryStep, error) {
	var state deliveryStep
	err := c.db.QueryRowContext(ctx, `SELECT id,step,room,fingerprint,payload,uuid,state,result,first_attempt,updated_at,message,status,code,request_id,retry_at
		FROM feishu_deliveries WHERE id=? AND step=?`, id, step).Scan(
		&state.ID, &state.Step, &state.Room, &state.Fingerprint, &state.Payload, &state.UUID, &state.State,
		&state.Result, &state.FirstAttempt, &state.UpdatedAt, &state.Message, &state.Status, &state.Code, &state.RequestID, &state.RetryAt)
	return state, err
}

func (s deliveryStep) failure(operation string) *APIError {
	e := &APIError{Operation: operation, Status: s.Status, Code: s.Code, RequestID: s.RequestID,
		Uncertain: s.State == "attempting" || s.State == "uncertain", Retryable: s.State == "retryable"}
	if s.RetryAt > 0 {
		e.RetryAfter = max(time.Duration(0), time.Until(time.UnixMilli(s.RetryAt)))
	}
	return e
}

// deliveryStep commits an immutable logical request before calling the provider,
// and commits its acknowledgement before returning. Card creation and message
// association are independent steps: CardKit creation has no supported UUID.
func (c *Client) deliveryStep(ctx context.Context, room, step string, payload core.Args, withUUID bool, send func(context.Context, core.Args, string) (string, error)) (string, error) {
	if c.isClosed() {
		return "", errors.New("feishu: transport is closed")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	key := core.DeliveryID(ctx)
	uuid := ""
	if withUUID {
		uuid = core.NewID()
	}
	if key == "" {
		return send(ctx, payload, uuid)
	}
	if !validDeliveryID(key) || c.db == nil {
		return "", errors.New("feishu: durable delivery requires a valid ID and database")
	}
	encoded, err := canonicalJSON(payload)
	if err != nil || len(encoded) > maxJSONBytes {
		return "", errors.New("feishu: invalid delivery payload")
	}
	fingerprint := core.Hash(encoded)
	unlock := c.locks.lock("delivery:" + key + ":" + step)
	defer unlock()
	now := time.Now().UnixMilli()
	_, err = c.db.ExecContext(ctx, `INSERT INTO feishu_deliveries(id,step,room,fingerprint,payload,uuid,state,updated_at)
		VALUES(?,?,?,?,?,?,'ready',?) ON CONFLICT(id,step) DO NOTHING`, key, step, room, fingerprint, encoded, uuid, now)
	if err != nil {
		return "", errors.New("feishu: delivery reservation failed")
	}
	saved, err := c.loadDelivery(ctx, key, step)
	if err != nil {
		return "", errors.New("feishu: delivery state is unavailable")
	}
	if saved.Room != room || saved.Fingerprint != fingerprint || saved.Payload != encoded {
		return "", errors.New("feishu: logical delivery payload changed")
	}
	if saved.State == "confirmed" {
		if !validID(saved.Result) {
			return "", &APIError{Operation: "saved delivery acknowledgement", Uncertain: true}
		}
		return saved.Result, nil
	}
	if (saved.State == "attempting" || saved.State == "uncertain") && !core.IsExplicitRetry(ctx) {
		return "", saved.failure("uncertain delivery requires explicit retry")
	}
	if saved.State == "rejected" && !core.IsExplicitRetry(ctx) {
		return "", saved.failure("delivery was rejected")
	}
	if saved.State == "retryable" && saved.RetryAt > now {
		return "", saved.failure("delivery is waiting for provider retry deadline")
	}
	if saved.State != "ready" && saved.State != "retryable" && saved.State != "rejected" && saved.State != "attempting" && saved.State != "uncertain" {
		return "", errors.New("feishu: invalid delivery state")
	}
	if (saved.State == "attempting" || saved.State == "uncertain") && core.IsExplicitRetry(ctx) {
		c.emit("delivery_explicit_replay", core.Args{"step": step, "afterDedupWindow": saved.FirstAttempt > 0 && time.Since(time.UnixMilli(saved.FirstAttempt)) >= messageDedupWindow, "providerDedup": saved.UUID != ""})
	}
	// A random attempt token prevents a late completion from overwriting another
	// explicitly authorized attempt, including across clients sharing a database.
	attempt := core.NewID()
	result, err := c.db.ExecContext(ctx, `UPDATE feishu_deliveries SET state='attempting',attempt=?,updated_at=?,
		first_attempt=CASE WHEN first_attempt=0 THEN ? ELSE first_attempt END
		WHERE id=? AND step=? AND state=? AND updated_at=?`, attempt, now, now, key, step, saved.State, saved.UpdatedAt)
	if err != nil {
		return "", errors.New("feishu: delivery attempt reservation failed")
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return "", &APIError{Operation: "concurrent delivery attempt", Uncertain: true}
	}
	var frozen core.Args
	if json.Unmarshal([]byte(saved.Payload), &frozen) != nil {
		return "", &APIError{Operation: "saved delivery payload", Uncertain: true}
	}
	id, sendErr := send(ctx, frozen, saved.UUID)
	if c.isClosed() {
		return "", &APIError{Operation: "delivery interrupted by shutdown", Uncertain: true}
	}
	state, status, code, requestID, retryAt := "confirmed", 0, 0, "", int64(0)
	if sendErr != nil {
		state = "rejected"
		var failure *APIError
		if errors.As(sendErr, &failure) {
			status, code, requestID = failure.Status, failure.Code, failure.RequestID
			if failure.Uncertain {
				state = "uncertain"
			} else if failure.DeliveryRetryable() {
				state = "retryable"
				retryAt = time.Now().Add(failure.DeliveryRetryAfter()).UnixMilli()
			}
		} else if errors.Is(sendErr, context.Canceled) || errors.Is(sendErr, context.DeadlineExceeded) {
			// The HTTP boundary always wraps a dispatched request in APIError.
			// A plain cancellation here happened before the mutation was sent.
			state = "retryable"
			sendErr = &APIError{Operation: "delivery canceled before dispatch", Retryable: true, cause: sendErr}
		}
	} else if !validID(id) {
		state = "uncertain"
		sendErr = &APIError{Operation: "delivery acknowledgement", Uncertain: true}
	}
	message := ""
	if state == "confirmed" && step == "message" {
		message = id
	}
	// Persist a cancellation outcome even when the caller stopped waiting. This is
	// bounded and still respects Close; it does not issue another network request.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	result, err = c.db.ExecContext(persistCtx, `UPDATE feishu_deliveries SET state=?,result=?,message=?,updated_at=?,status=?,code=?,request_id=?,retry_at=?
		WHERE id=? AND step=? AND state='attempting' AND attempt=?`, state, id, message, time.Now().UnixMilli(), status, code, requestID, retryAt, key, step, attempt)
	if err != nil {
		return "", &APIError{Operation: "delivery acknowledgement persistence", Uncertain: true}
	}
	count, err = result.RowsAffected()
	if err != nil || count != 1 {
		return "", &APIError{Operation: "delivery acknowledgement ownership", Uncertain: true}
	}
	if sendErr != nil {
		return "", localRejection("delivery rejected before dispatch", sendErr)
	}
	return id, nil
}

func (c *Client) sendJournaledJSON(ctx context.Context, room, step, method, path string, query url.Values, body core.Args, resultField string, withUUID bool) (string, error) {
	payload := core.Args{"method": method, "path": path, "query": query.Encode(), "body": body}
	return c.deliveryStep(ctx, room, step, payload, withUUID, func(attemptCtx context.Context, frozen core.Args, uuid string) (string, error) {
		requestBody, ok := object(frozen["body"])
		if !ok {
			return "", errors.New("feishu: invalid saved request body")
		}
		if withUUID {
			requestBody["uuid"] = uuid
		}
		values, err := url.ParseQuery(stringValue(frozen, "query"))
		if err != nil {
			return "", errors.New("feishu: invalid saved request query")
		}
		var result core.Args
		if err = c.api(attemptCtx, stringValue(frozen, "method"), stringValue(frozen, "path"), values, requestBody, &result); err != nil {
			return "", err
		}
		id := stringValue(result, resultField)
		if !validID(id) {
			return "", &APIError{Operation: "delivery acknowledgement", Uncertain: true}
		}
		return id, nil
	})
}

func (c *Client) linkDeliveryMessage(ctx context.Context, message string) error {
	if c.db == nil || core.DeliveryID(ctx) == "" {
		return nil
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE feishu_deliveries SET message=? WHERE id=? AND state='confirmed'`, message, core.DeliveryID(ctx)); err != nil {
		return &APIError{Operation: "delivery message association persistence", Uncertain: true}
	}
	return nil
}

// hasOutbox keeps the transport usable with its small SQLite integration
// fixture and with databases opened by older installations before migration.
func (c *Client) hasOutbox(ctx context.Context) (bool, error) {
	var name string
	err := c.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name='outbox'`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
