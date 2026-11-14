package core

// Control request kinds share one local control directory. Older attach
// requests have no kind and are treated as ControlKindAttach.
const (
	ControlKindAttach = "attach"
	ControlKindNotify = "notify"
	ControlKindReply  = "reply"
)

// Stable machine-readable error codes for notify and topic reply. The harness
// falls back to the legacy DM notification only for not_configured and
// gateway_unavailable; the other codes must be reported without resending.
const (
	NotifyErrorNotConfigured      = "not_configured"
	NotifyErrorGatewayUnavailable = "gateway_unavailable"
	NotifyErrorInvalidRequest     = "invalid_request"
	NotifyErrorTransportFailure   = "transport_failure"
	NotifyErrorPending            = "pending"
	NotifyErrorUncertain          = "uncertain"
)

// AdoptError is the nested error object shared by notify and topic reply.
type AdoptError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// NotifyRequest sends a task-result summary to the owner's adopted DM topic.
// The first call creates the topic; later calls append one reply. Messaging
// fields register the live CC inbox used for interactive relay.
type NotifyRequest struct {
	Kind            string `json:"kind,omitempty"`
	ID              string `json:"id"`
	At              int64  `json:"at"`
	User            string `json:"user"`
	Native          string `json:"native"`
	CWD             string `json:"cwd,omitempty"`
	HolderPID       int    `json:"holderPid,omitempty"`
	HolderStarted   int64  `json:"holderStarted,omitempty"`
	Title           string `json:"title,omitempty"`
	Summary         string `json:"summary,omitempty"`
	ConfigPath      string `json:"configPath,omitempty"`
	MessagingSocket string `json:"messagingSocket,omitempty"`
	MessagingToken  string `json:"messagingToken,omitempty"`
}

// NotifyResult uses snake_case because it is consumed by the external harness.
type NotifyResult struct {
	OK            bool        `json:"ok"`
	State         string      `json:"state,omitempty"`
	ChatID        string      `json:"chat_id,omitempty"`
	RootMessageID string      `json:"root_message_id,omitempty"`
	ThreadID      string      `json:"thread_id,omitempty"`
	MessageID     string      `json:"message_id,omitempty"`
	Created       bool        `json:"created"`
	Warnings      []string    `json:"warnings,omitempty"`
	Error         *AdoptError `json:"error,omitempty"`
}

// TopicReplyRequest posts one local session answer back to the topic of the
// forwarded Feishu message identified by MessageID.
type TopicReplyRequest struct {
	Kind       string `json:"kind,omitempty"`
	ID         string `json:"id"`
	At         int64  `json:"at"`
	Native     string `json:"native"`
	MessageID  string `json:"messageId"`
	Text       string `json:"text"`
	ConfigPath string `json:"configPath,omitempty"`
}

// TopicReplyResult reports the delivered topic message.
type TopicReplyResult struct {
	OK        bool        `json:"ok"`
	MessageID string      `json:"message_id,omitempty"`
	Error     *AdoptError `json:"error,omitempty"`
}
