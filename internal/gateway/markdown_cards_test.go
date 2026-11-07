package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/feishu"
)

func replyMarkdownContent(t *testing.T, card core.Args) string {
	t.Helper()
	elements, _ := argsOf(card["body"])["elements"].([]any)
	for _, item := range elements {
		element := argsOf(item)
		if element["element_id"] == "reply_answer" {
			return stringArg(element, "content")
		}
	}
	t.Fatal("missing reply answer component")
	return ""
}

func exampleMarkdownTables(n int) string {
	var text strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&text, "| 项目 %d | 内容 |\n| --- | :---: |\n| 数据 | 中文 🐱 |\n\n", i)
	}
	return text.String()
}

func TestReplyMarkdownKeepsFourTablesAndDegradesOverflow(t *testing.T) {
	answer := exampleMarkdownTables(6) + "后续说明。"
	for _, final := range []bool{false, true} {
		card, needsFile := boundedReplyCard("回复", answer, nil, 0, final, replyDetails{}, time.Second, true, nil)
		if !needsFile {
			t.Fatal("table overflow did not retain the original as a required file")
		}
		text := replyMarkdownContent(t, card)
		if !strings.HasPrefix(text, exampleMarkdownTables(4)) {
			t.Fatal("the first four tables lost their normal Markdown presentation")
		}
		if !strings.Contains(text, "```\n| 项目 5 |") || !strings.Contains(text, "后续说明。\n```") {
			t.Fatalf("overflow is not preserved as literal text: %q", text)
		}
		if !strings.Contains(text, "完整答案") {
			t.Fatal("missing original-file notice")
		}
	}
}

func TestReplyMarkdownDoesNotCountCodeOrNonTablePipes(t *testing.T) {
	code := "````markdown\n```go\n" + exampleMarkdownTables(6) + "```\n````\n\n" +
		"~~~text\n" + exampleMarkdownTables(6) + "~~~\n\n" +
		"    | code | sample |\n    | --- | --- |\n\n" +
		"not \\| a table\n--- | ---\n\n| prose | only |\n| still | prose |\n\n"
	answer := code + exampleMarkdownTables(4)
	card, needsFile := boundedReplyCard("回复", answer, nil, 0, true, replyDetails{}, time.Second, true, nil)
	if needsFile || replyMarkdownContent(t, card) != answer {
		t.Fatal("code samples or ordinary pipes consumed the table budget")
	}
}

func TestReplyMarkdownKeepsDelimiterLikeRowsInOneTable(t *testing.T) {
	answer := strings.ReplaceAll(exampleMarkdownTables(4), "| 数据 | 中文 🐱 |", "| 数据 | 中文 🐱 |\n| --- | --- |")
	card, needsFile := boundedReplyCard("回复", answer, nil, 0, true, replyDetails{}, time.Second, true, nil)
	if needsFile || replyMarkdownContent(t, card) != answer {
		t.Fatal("a delimiter-like data row was counted as another table")
	}
}

func TestReplyMarkdownClosesOnlyOpenFencesInPreview(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"backticks", "```go\nfmt.Println(\"中文\")", "```go\nfmt.Println(\"中文\")\n```"},
		{"long-fence", "````markdown\n```go\ncode\n```", "````markdown\n```go\ncode\n```\n````"},
		{"tilde", "~~~text\npartial", "~~~text\npartial\n~~~"},
		{"closed", "```go\ncode\n```\n", "```go\ncode\n```\n"},
		{"inline", "Use `a | b` here.", "Use `a | b` here."},
		{"quote", "> ```go\n> code", "> ```go\n> code\n> ```"},
		{"quote-newline", "> ```go\n> code\n", "> ```go\n> code\n> ```"},
		{"quoted-code-content", "```markdown\n> ```\npartial", "```markdown\n> ```\npartial\n```"},
		{"indented-close", "```markdown\n    ```\npartial", "```markdown\n    ```\npartial\n```"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			card, needsFile := boundedReplyCard("处理中", tc.input, nil, 0, false, replyDetails{}, 0, false, nil)
			if needsFile || replyMarkdownContent(t, card) != tc.want {
				t.Fatalf("preview=%q, file=%t; want %q", replyMarkdownContent(t, card), needsFile, tc.want)
			}
		})
	}
}

func TestReplyMarkdownTruncationClosesFenceBeforeNotice(t *testing.T) {
	for _, fence := range []string{"```", strings.Repeat("`", 14000), "~~~"} {
		answer := fence + "\n" + strings.Repeat("中文 🐱 \"\\\n", 9000)
		card, needsFile := boundedReplyCard("回复", answer, nil, 0, true, replyDetails{}, time.Second, true, nil)
		text := replyMarkdownContent(t, card)
		size, err := core.CardPayloadBytes(card)
		if err != nil || size > core.CardPayloadLimit || !utf8.ValidString(text) || !needsFile {
			t.Fatalf("invalid preview budget/Unicode: bytes=%d err=%v file=%t", size, err, needsFile)
		}
		body, _, ok := strings.Cut(text, "\n\n（完整答案另附文件。）")
		if !ok {
			t.Fatal("missing truncation notice")
		}
		opening, _, _ := strings.Cut(body, "\n")
		if len(opening) >= 3 && !strings.HasSuffix(body, "\n"+opening) {
			t.Fatalf("notice is inside an unterminated code fence (opening length %d)", len(opening))
		}
	}
}

func TestNoticeBudgetKeepsCodeFenceClosed(t *testing.T) {
	card := replyCard("回复", "```go\n"+strings.Repeat("中文\n", 2000)+"```", nil, 0, true, replyDetails{}, 0, true)
	card = fitCardBudget(card, 4000, "\n\n预览已截断。")
	text := replyMarkdownContent(t, card)
	if !strings.HasSuffix(text, "\n```\n\n预览已截断。") {
		t.Fatal("delivery status budgeting cut the closing fence or hid the notice")
	}
	if size, err := core.CardPayloadBytes(card); err != nil || size > 4000 {
		t.Fatalf("notice exceeds budget: %d %v", size, err)
	}
}

func TestMarkdownOverflowExportsOriginalBeforeDone(t *testing.T) {
	f := newFixture(t, nil)
	answer := exampleMarkdownTables(5) + "原始 **完整** 答案。"
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	f.tr.fileHook = func(ctx context.Context, path string) (string, error) {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != answer {
			t.Errorf("export changed the original Markdown: %v", err)
		}
		close(started)
		select {
		case <-release:
			return "markdown-original", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f.send(f.message("markdown", "compare the tables", "member"))
	w := f.worker()
	d := f.delivery(w)
	f.finish(w, d, answer)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("original Markdown attachment not delivered")
	}
	var saved string
	if err := f.s.DB.QueryRow("SELECT text FROM outbox WHERE kind='reply' AND room=?", f.room().Key).Scan(&saved); err != nil || saved != answer {
		t.Fatalf("persisted answer was rewritten: %v", err)
	}
	if f.tr.receiptDone("markdown") {
		t.Fatal("Done before original-file acknowledgement")
	}
	once.Do(func() { close(release) })
	eventually(t, func() bool { return f.tr.receiptDone("markdown") })
	if f.tr.cardCount() != 1 {
		t.Fatal("fallback replaced the reply card")
	}
}

// Exercise the real CardKit element-content path, not only the card builder.
func TestMarkdownPreviewReachesCardKitStreamAndKeepsCard(t *testing.T) {
	f := newFixture(t, nil)
	f.c.AppSecretEnv = "FEISHU_MARKDOWN_FIXTURE_SECRET"
	t.Setenv(f.c.AppSecretEnv, "fixture-secret")
	var paths []string
	var streamed string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			_ = json.NewEncoder(w).Encode(core.Args{"code": 0, "tenant_access_token": "fixture-token", "expire": 7200})
			return
		}
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		var body core.Args
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		data := core.Args{}
		switch r.URL.Path {
		case "/open-apis/cardkit/v1/cards":
			data["card_id"] = "markdown-card"
		case "/open-apis/im/v1/messages":
			data["message_id"] = "markdown-message"
		case "/open-apis/cardkit/v1/cards/markdown-card/elements/reply_answer/content":
			streamed = stringArg(body, "content")
		}
		_ = json.NewEncoder(w).Encode(core.Args{"code": 0, "data": data})
	}))
	t.Cleanup(server.Close)
	client, err := feishu.New(f.c, f.s.DB, nil, feishu.Options{BaseURL: server.URL, HTTPClient: server.Client(), BotOpenID: "bot"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	card := replyCard("处理中", "start", nil, 0, false, replyDetails{}, 0, false)
	id, err := client.Card(context.Background(), f.room(), card, "", "")
	if err != nil {
		t.Fatal(err)
	}
	card = replyCard("处理中", "```go\n中文", nil, 0, false, replyDetails{}, 0, false)
	if err := client.Update(context.Background(), id, card); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	actual := streamed
	mu.Unlock()
	if actual != "```go\n中文\n```" {
		t.Fatalf("stream bypassed Markdown preparation: %q", actual)
	}
	answer := "```go\n中文\n```\n\n" + exampleMarkdownTables(5)
	card = replyCard("处理中", answer, nil, 0, false, replyDetails{}, 0, false)
	if err := client.Update(context.Background(), id, card); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	actual = streamed
	mu.Unlock()
	if !strings.Contains(actual, "```\n| 项目 5 |") || !strings.Contains(actual, "完成后附完整答案文件") {
		t.Fatalf("stream bypassed the table limit: %q", actual)
	}
	card = replyCard("回复", answer, nil, 0, true, replyDetails{}, 0, true)
	if err := client.Update(context.Background(), id, card); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 5 || paths[2] != "PUT /open-apis/cardkit/v1/cards/markdown-card/elements/reply_answer/content" || paths[3] != paths[2] || paths[4] != "PUT /open-apis/cardkit/v1/cards/markdown-card" {
		t.Fatalf("unexpected delivery/finalization sequence: %#v", paths)
	}
}
