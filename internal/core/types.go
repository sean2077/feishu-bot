// Package core defines the durable conversation and adapter contracts.
package core

import "context"

type Args = map[string]any

type Resource struct {
	Key  string `json:"key"`
	Kind string `json:"kind"`
	Name string `json:"name"`
}
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
	Mentioned            bool       `json:"mentioned"`
	ReplyTo              string     `json:"replyTo,omitempty"`
	InteractionMessageID string     `json:"interactionMessageId,omitempty"`
	ThreadID             string     `json:"threadId,omitempty"`
	RootID               string     `json:"rootId,omitempty"`
	Resources            []Resource `json:"resources"`
	InputIssue           string     `json:"inputIssue,omitempty"`
	ImageOnly            bool       `json:"imageOnly,omitempty"`
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
	Message      Message      `json:"message"`
	Background   []Message    `json:"background"`
	Quote        *Message     `json:"quote,omitempty"`
	Command      *Command     `json:"command,omitempty"`
	SupplementTo string       `json:"supplementTo,omitempty"`
	ImageBatch   *ImageBatch  `json:"imageBatch,omitempty"`
	BatchInputs  []Delivery   `json:"batchInputs,omitempty"`
	Attachments  []Attachment `json:"attachments"`
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
}
type Project struct {
	ID               string       `json:"id"`
	Maintainers      []string     `json:"maintainers"`
	SeedDir          string       `json:"seedDir,omitempty"`
	TrustedConfigDir string       `json:"trustedConfigDir,omitempty"`
	CWD              string       `json:"cwd,omitempty"`
	NativeConfig     string       `json:"nativeConfig,omitempty"`
	PermissionMode   string       `json:"permissionMode"`
	AllowedTools     []string     `json:"allowedTools"`
	Actions          []ActionSpec `json:"actions"`
}
type RuntimeConfig struct {
	MaxWorkers       int    `json:"maxWorkers"`
	IdleMS           int64  `json:"idleMs"`
	ClaudeExecutable string `json:"claudeExecutable,omitempty"`
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
	ChatID          string   `json:"chatId"`
	ProjectID       string   `json:"projectId"`
	Members         []string `json:"members,omitempty"`
	MentionOptional bool     `json:"mentionOptional"`
	AllowAllMembers bool     `json:"allowAllMembers,omitempty"`
	SessionScope    string   `json:"sessionScope,omitempty"`
}
type DM struct {
	UserID    string `json:"userId"`
	ProjectID string `json:"projectId"`
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
	DataDir       string                  `json:"dataDir"`
	Runtime       RuntimeConfig           `json:"runtime"`
	Model         ModelConfig             `json:"model"`
	ModelProfiles map[string]ModelProfile `json:"modelProfiles,omitempty"`
	Projects      []Project               `json:"projects"`
	Groups        []Group                 `json:"groups"`
	DMs           []DM                    `json:"dms"`
	Limits        Limits                  `json:"limits"`
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
	Background       *int     `json:"background,omitempty"`
	Pinned           *bool    `json:"pinned,omitempty"`
	State            string   `json:"state,omitempty"`
	ID               string   `json:"id,omitempty"`
	Kind             string   `json:"kind,omitempty"`
	Name             string   `json:"name,omitempty"`
	Input            Args     `json:"input,omitempty"`
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
type Workspace struct {
	Root, Work, Home, Inbound string
	NativeConfig, ConfigDir   string
	ConfigDirOverride         bool
}
type WorkerFactory func(context.Context, Room, Session, Workspace, func(WorkerEvent)) (Worker, error)
type Logger func(string, Args)
