package core

import "context"

// Adopted topic states are also written to the adopted_topics table. They are
// repeated here so the CLI, transport and gateway share the report vocabulary
// without importing the store.
const (
	AdoptionPending   = "pending"
	AdoptionOpen      = "open"
	AdoptionUncertain = "uncertain"
)

// Adoption limits keep the root card and first topic reply bounded.
const (
	AdoptTitleRunes   = 80
	AdoptSummaryRunes = 6000
)

// AttachRequest is the local control-channel request that adopts one running
// Claude Code session into a Feishu DM topic. It is written by the CLI below
// the gateway data directory (mode 0700) and never contains credentials.
type AttachRequest struct {
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

// AttachResult is the gateway's answer to AttachRequest. State is empty for a
// rejected request; pending and uncertain results are not success and must not
// cause the caller to replay the external write.
type AttachResult struct {
	OK            bool     `json:"ok"`
	State         string   `json:"state,omitempty"`
	Room          string   `json:"room,omitempty"`
	ChatID        string   `json:"chatId,omitempty"`
	RootMessageID string   `json:"rootMessageId,omitempty"`
	ThreadID      string   `json:"threadId,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
	Error         string   `json:"error,omitempty"`
}

// UserCardSender sends one interactive card to a user's private chat by
// open_id and reports the created message and its chat ID. The adoption flow
// needs the chat ID before any DM chat event has identified it.
type UserCardSender interface {
	SendUserCard(context.Context, string, Args) (messageID, chatID string, err error)
}
