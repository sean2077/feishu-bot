// Package core defines the durable conversation and adapter contracts.
package core

import "context"

type Args = map[string]any

type Resource struct {
	Key  string `json:"key"`
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// HistoryCardPlaceholder is the text some Feishu clients/API versions return
// for interactive cards they cannot render from history.
const HistoryCardPlaceholder = "请升级至最新版本客户端，以查看内容"

type Message struct {
	ID                   string     `json:"id"`
	ChatID               string     `json:"chatId"`
	ChatType             string     `json:"chatType"`
	SenderID             string     `json:"senderId"`
	SenderName           string     `json:"senderName,omitempty"`
	SenderIsBot          bool       `json:"senderIsBot,omitempty"`
	SenderIsSelf         bool       `json:"senderIsSelf,omitempty"`
	Text                 string     `json:"text"`
	Time                 int64      `json:"time"`
	LocalTime            string     `json:"localTime,omitempty"`
	Mentioned            bool       `json:"mentioned"`
	ReplyTo              string     `json:"replyTo,omitempty"`
	InteractionMessageID string     `json:"interactionMessageId,omitempty"`
	ThreadID             string     `json:"threadId,omitempty"`
	RootID               string     `json:"rootId,omitempty"`
	Resources            []Resource `json:"resources"`
	InputIssue           string     `json:"inputIssue,omitempty"`
	ImageOnly            bool       `json:"imageOnly,omitempty"`
	// Background marks history from another topic or the chat timeline. It is
	// data, not a request or authorization.
	Background bool `json:"background,omitempty"`
}
type Room struct {
	Key             string   `json:"key"`
	ChatID          string   `json:"chatId"`
	ProjectID       string   `json:"projectId"`
	DMUser          string   `json:"dmUser,omitempty"`
	Maintainers     []string `json:"maintainers"`
	Members         []string `json:"members,omitempty"`
	MentionOptional bool     `json:"mentionOptional"`
	AllowAllMembers bool     `json:"allowAllMembers,omitempty"`
	// SessionScope is the configured group scope. Thread is the topic root
	// message of a thread-scoped room; ChatKey is the chat room key shared by
	// every topic of that chat for workspace and native HOME placement.
	SessionScope string `json:"sessionScope,omitempty"`
	Thread       string `json:"thread,omitempty"`
	ChatKey      string `json:"chatKey,omitempty"`
	// WorkspaceCWD is an administrator-configured topic directory. It changes
	// the native working directory, never the room key or native HOME placement.
	WorkspaceCWD string `json:"workspaceCwd,omitempty"`
	// TopicHistory is the group setting that permits scope=chat history reads.
	TopicHistory string `json:"topicHistory,omitempty"`
}
type Session struct {
	ID        string `json:"id"`
	RoomKey   string `json:"roomKey"`
	CCID      string `json:"ccId,omitempty"`
	State     string `json:"state"`
	Profile   string `json:"profile,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}
type ImageBatch struct {
	ID        string `json:"id"`
	Deadline  int64  `json:"deadline"`
	Closed    bool   `json:"closed"`
	MaxImages int    `json:"maxImages"`
	MaxBytes  int64  `json:"maxBytes"`
}
type Command struct {
	Name         string `json:"name"`
	Instructions string `json:"instructions"`
}
type Attachment struct {
	Path string `json:"path"`
	Name string `json:"name"`
}
type Delivery struct {
	UUID         string       `json:"uuid"`
	RoomKey      string       `json:"roomKey"`
	SessionID    string       `json:"sessionId"`
	QueuedAt     int64        `json:"queuedAt,omitempty"`
	Message      Message      `json:"message"`
	Background   []Message    `json:"background"`
	Quote        *Message     `json:"quote,omitempty"`
	Command      *Command     `json:"command,omitempty"`
	SupplementTo string       `json:"supplementTo,omitempty"`
	ImageBatch   *ImageBatch  `json:"imageBatch,omitempty"`
	BatchInputs  []Delivery   `json:"batchInputs,omitempty"`
	Attachments  []Attachment `json:"attachments"`
	// PreContext is populated by the gateway only for the input that starts a
	// new turn. It is never user input or authorization.
	PreContext *PreContext `json:"preContext,omitempty"`
}

// RequestContext describes the human inputs active in a room when a request
// arrives. It comes from the gateway's current deliveries and anchor, never
// from model-reported values. Senders and message IDs follow delivery order.
type RequestContext struct {
	ChatID       string   `json:"chatId"`
	ChatType     string   `json:"chatType"`
	ThreadID     string   `json:"threadId"`
	MessageID    string   `json:"messageId"`
	SenderOpenID string   `json:"senderOpenId"`
	SenderName   string   `json:"senderName"`
	Senders      []string `json:"senders"`
	MessageIDs   []string `json:"messageIds"`
}

// RequesterLabel prefers the event-provided display name and falls back to the
// open ID. It never calls the contact API.
func (c RequestContext) RequesterLabel() string {
	if c.SenderName != "" {
		return c.SenderName
	}
	return c.SenderOpenID
}

type Parameter struct {
	Description   string   `json:"description,omitempty"`
	Pattern       string   `json:"pattern,omitempty"`
	Enum          []string `json:"enum,omitempty"`
	MaxLength     int      `json:"maxLength"`
	WorkspaceFile bool     `json:"workspaceFile,omitempty"`
}
type ActionSpec struct {
	ID          string               `json:"id"`
	Description string               `json:"description"`
	Executable  string               `json:"executable"`
	Args        []string             `json:"args"`
	CWD         string               `json:"cwd,omitempty"`
	Env         map[string]string    `json:"env"`
	Vars        map[string]string    `json:"vars"`
	ReadOnly    bool                 `json:"readOnly"`
	TimeoutMS   int64                `json:"timeoutMs"`
	Parameters  map[string]Parameter `json:"parameters"`
	Approvers   []string             `json:"approvers,omitempty"`
}

// ProjectApprovals holds per-project approval policy. The zero value keeps
// the existing maintainer-only behavior.
type ProjectApprovals struct {
	ForbidSelfApproval bool `json:"forbidSelfApproval,omitempty"`
}

// FeedbackConfig enables reply-card feedback for a project.
type FeedbackConfig struct {
	Enabled bool `json:"enabled,omitempty"`
}

// FeedbackExportConfig writes per-project daily feedback archives before the
// feedback retention policy can delete those rows. It is disabled by default.
type FeedbackExportConfig struct {
	Enabled bool   `json:"enabled,omitempty"`
	Dir     string `json:"dir,omitempty"`
}

// FeedbackSettings holds deployment-wide feedback options. Project-level
// collection remains in Project.Feedback.
type FeedbackSettings struct {
	Export FeedbackExportConfig `json:"export,omitempty"`
}

// ProjectBash holds native Bash hook preferences. The zero value keeps the
// existing hook behavior.
type ProjectBash struct {
	RejectCwdPrefix bool `json:"rejectCwdPrefix,omitempty"`
}

// ProjectPreContext configures one administrator-approved read-only action
// that the gateway runs before a new human turn. Its output is injected as
// untrusted background context, not as user input or authorization.
type ProjectPreContext struct {
	Action    string `json:"action"`
	Parameter string `json:"parameter"`
	TimeoutMS int64  `json:"timeoutMs,omitempty"`
	MaxChars  int    `json:"maxChars,omitempty"`
}

// PreContext is the gateway-produced retrieval result attached to one native
// input. Source identifies the configured action; the text is untrusted.
type PreContext struct {
	Source    string `json:"source"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

type Project struct {
	ID                     string             `json:"id"`
	Maintainers            []string           `json:"maintainers"`
	Owners                 []string           `json:"owners,omitempty"`
	Approvals              ProjectApprovals   `json:"approvals,omitempty"`
	Bash                   ProjectBash        `json:"bash,omitempty"`
	Feedback               FeedbackConfig     `json:"feedback,omitempty"`
	DisplayWorkspace       string             `json:"displayWorkspace,omitempty"`
	SeedDir                string             `json:"seedDir,omitempty"`
	TrustedConfigDir       string             `json:"trustedConfigDir,omitempty"`
	CWD                    string             `json:"cwd,omitempty"`
	SettingsFile           string             `json:"settingsFile,omitempty"`
	AppendSystemPrompt     string             `json:"appendSystemPrompt,omitempty"`
	AppendSystemPromptFile string             `json:"appendSystemPromptFile,omitempty"`
	PreContext             *ProjectPreContext `json:"preContext,omitempty"`
	NativeConfig           string             `json:"nativeConfig,omitempty"`
	ModelProfile           string             `json:"modelProfile,omitempty"`
	PermissionMode         string             `json:"permissionMode"`
	AllowedTools           []string           `json:"allowedTools"`
	Actions                []ActionSpec       `json:"actions"`
	// Env holds non-secret literal values for the native process and its
	// descendants. EnvPassthrough names variables copied from the service
	// environment; absent or empty values are not injected.
	Env            map[string]string `json:"env,omitempty"`
	EnvPassthrough []string          `json:"envPassthrough,omitempty"`
}
type RuntimeConfig struct {
	MaxWorkers        int    `json:"maxWorkers"`
	MaxWorkersPerChat int    `json:"maxWorkersPerChat,omitempty"`
	MaxRSSMiB         int64  `json:"maxRssMiB,omitempty"`
	IdleMS            int64  `json:"idleMs"`
	IdleMinMS         int64  `json:"idleMinMs,omitempty"`
	ClaudeExecutable  string `json:"claudeExecutable,omitempty"`
}
type ModelConfig struct {
	BaseURL      string            `json:"baseUrl"`
	TokenEnv     string            `json:"tokenEnv"`
	Model        string            `json:"model"`
	SmallModel   string            `json:"smallModel"`
	Effort       string            `json:"effort,omitempty"`
	ExtraHeaders map[string]string `json:"extraHeaders"`
	AllowHTTP    bool              `json:"allowHttp"`
}

// ModelProfile changes only native model selection. Credentials, endpoints and
// permission policy remain deployment settings, never chat-controlled values.
type ModelProfile struct {
	Model      string `json:"model"`
	SmallModel string `json:"smallModel,omitempty"`
	Effort     string `json:"effort,omitempty"`
}
type Group struct {
	ChatID           string            `json:"chatId"`
	ProjectID        string            `json:"projectId"`
	Members          []string          `json:"members,omitempty"`
	MentionOptional  bool              `json:"mentionOptional"`
	AllowAllMembers  bool              `json:"allowAllMembers,omitempty"`
	SessionScope     string            `json:"sessionScope,omitempty"`
	TopicHistory     string            `json:"topicHistory,omitempty"`
	ThreadWorkspaces map[string]string `json:"threadWorkspaces,omitempty"`
}
type HistoryChat struct {
	ChatID string `json:"chatId"`
	Name   string `json:"name"`
}

type DM struct {
	UserID       string        `json:"userId"`
	ProjectID    string        `json:"projectId"`
	HistoryChats []HistoryChat `json:"historyChats,omitempty"`
}
type Limits struct {
	BackgroundMessages  int   `json:"backgroundMessages"`
	BackgroundMS        int64 `json:"backgroundMs"`
	TextChars           int   `json:"textChars"`
	InboxPerRoom        int   `json:"inboxPerRoom"`
	AttachmentBytes     int64 `json:"attachmentBytes"`
	ApprovalMS          int64 `json:"approvalMs"`
	UpdateMS            int64 `json:"updateMs"`
	MessagesPerMinute   int   `json:"messagesPerMinute"`
	ImageBatchMS        int64 `json:"imageBatchMs"`
	ImageBatchMaxImages int   `json:"imageBatchMaxImages"`
	ImageBatchBytes     int64 `json:"imageBatchBytes"`
}
type Config struct {
	TenantID      string                  `json:"tenantId"`
	AppID         string                  `json:"appId"`
	AppSecretEnv  string                  `json:"appSecretEnv"`
	CardTitle     string                  `json:"cardTitle,omitempty"`
	DataDir       string                  `json:"dataDir"`
	Runtime       RuntimeConfig           `json:"runtime"`
	Model         ModelConfig             `json:"model"`
	ModelProfiles map[string]ModelProfile `json:"modelProfiles,omitempty"`
	Projects      []Project               `json:"projects"`
	Groups        []Group                 `json:"groups"`
	DMs           []DM                    `json:"dms"`
	Limits        Limits                  `json:"limits"`
	Feedback      FeedbackSettings        `json:"feedback,omitempty"`
}

func (c Config) Project(id string) (Project, bool) {
	for _, p := range c.Projects {
		if p.ID == id {
			return p, true
		}
	}
	return Project{}, false
}

type HistoryQuery struct {
	Limit               int
	StartTime, EndTime  int64
	PageToken, ThreadID string
	Scope               string `json:"scope,omitempty"`
	ExpandThreads       bool   `json:"expandThreads,omitempty"`
	Chat                string `json:"chat,omitempty"`
}
type HistoryPage struct {
	Messages      []Message
	NextPageToken string
}
type Origin struct {
	ID       string `json:"id"`
	SenderID string `json:"senderId"`
}
type Transport interface {
	Send(context.Context, Room, string, string, string) (string, error)
	Card(context.Context, Room, Args, string, string) (string, error)
	Update(context.Context, string, Args) error
	Download(context.Context, string, Resource, string, int64) error
	Fetch(context.Context, string, Room) (*Message, error)
	History(context.Context, Room, HistoryQuery) (HistoryPage, error)
	Receipt(context.Context, string, string, string) error
	File(context.Context, Room, string, string) (string, error)
}
type CardAction struct {
	UserID    string
	ChatID    string
	MessageID string
	Option    string
	Value     Args
	FormValue Args
}
type WorkerEvent struct {
	Type             string   `json:"type"`
	CCID             string   `json:"ccId,omitempty"`
	UUID             string   `json:"uuid,omitempty"`
	UUIDs            []string `json:"uuids,omitempty"`
	Reason           string   `json:"reason,omitempty"`
	Text             string   `json:"text,omitempty"`
	Partial          bool     `json:"partial,omitempty"`
	BlockID          string   `json:"blockId,omitempty"`
	TurnID           string   `json:"turnId,omitempty"`
	Source           string   `json:"source,omitempty"`
	BackgroundID     string   `json:"backgroundId,omitempty"`
	Phase            string   `json:"phase,omitempty"`
	Trigger          string   `json:"trigger,omitempty"`
	Success          bool     `json:"success,omitempty"`
	Usage            Args     `json:"usage,omitempty"`
	Context          Args     `json:"context,omitempty"`
	ResultID         string   `json:"resultId,omitempty"`
	Models           []string `json:"models,omitempty"`
	Model            string   `json:"model,omitempty"`
	ReplyToUUID      string   `json:"replyToUuid,omitempty"`
	Compact          string   `json:"compact,omitempty"`
	Tool             string   `json:"tool,omitempty"`
	ToolUseID        string   `json:"toolUseId,omitempty"`
	StartupMS        int64    `json:"startupMs,omitempty"`
	PermissionMode   string   `json:"permissionMode,omitempty"`
	Stage2Error      bool     `json:"stage2Error,omitempty"`
	Background       *int     `json:"background,omitempty"`
	Pinned           *bool    `json:"pinned,omitempty"`
	State            string   `json:"state,omitempty"`
	ID               string   `json:"id,omitempty"`
	Kind             string   `json:"kind,omitempty"`
	Name             string   `json:"name,omitempty"`
	Input            Args     `json:"input,omitempty"`
	Tainted          bool     `json:"tainted,omitempty"`
	Message          string   `json:"message,omitempty"`
	Code             *int     `json:"code,omitempty"`
	CleanupConfirmed *bool    `json:"cleanupConfirmed,omitempty"`
}
type Worker interface {
	Send(context.Context, Delivery) error
	Respond(context.Context, string, Args) error
	Rename(context.Context, string) error
	Stop(context.Context) error
}

// RequestContextProvider is implemented by a native worker that exposes the
// gateway's live request context to its loopback context endpoint.
type RequestContextProvider interface {
	SetRequestContext(func() *RequestContext)
}

// TaintProvider is implemented by a native worker that checks whether the
// current conversation turn has read another conversation's content. The
// callback is read from the PreToolUse hook, including calls made by background
// tasks and subagents that outlive the turn.
type TaintProvider interface {
	SetTaintCheck(func() bool)
}

// WorkerEvents separates process termination from completion of its final
// callbacks. WaitEvents must be called outside a worker event callback.
type WorkerEvents interface {
	WaitEvents(context.Context) error
}
type Workspace struct {
	Root, Work, Home, Inbound string
	NativeConfig, ConfigDir   string
	ConfigDirOverride         bool
}
type WorkerFactory func(context.Context, Room, Session, Workspace, func(WorkerEvent)) (Worker, error)
type Logger func(string, Args)
