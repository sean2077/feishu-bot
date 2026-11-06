package gateway

import (
	"context"
	"strings"
	"testing"
	"time"
	"github.com/sean2077/feishu-bot/internal/core"
)

func TestSessionSearchCursorScopeTamperAndCurrentIdentity(t *testing.T) {
	f:=newFixture(t,nil);r:=f.room()
	for i:=0;i<24;i++ { if _,err:=f.s.NewSession(r.Key);err!=nil{t.Fatal(err)} }
	page,err:=f.g.sessionPage(r,"")
	if err!=nil||len(page.Items)!=20||page.NextCursor==""{t.Fatal("first page",page,err)}
	next,err:=f.g.sessionPage(r,"--cursor "+page.NextCursor)
	if err!=nil||len(next.Items)!=4||next.NextCursor!=""{t.Fatal("next page",next,err)}
	seen:=map[string]bool{};for _,s:=range page.Items{seen[s.ID]=true};for _,s:=range next.Items{if seen[s.ID]{t.Fatal("duplicate across pages")}}
	if _,err:=f.g.sessionPage(r,"changed --cursor "+page.NextCursor);err==nil{t.Fatal("cursor accepted for different query")}
	p,err:=f.g.readSessionCursor(page.NextCursor,r.Key,time.Now().UnixMilli());if err!=nil{t.Fatal(err)}
	if _,err:=f.g.readSessionCursor(page.NextCursor,"other",time.Now().UnixMilli());err==nil{t.Fatal("cross-room cursor accepted")}
	if _,err:=f.g.readSessionCursor(page.NextCursor+"x",r.Key,time.Now().UnixMilli());err==nil{t.Fatal("tampered cursor accepted")}
	if _,err:=f.g.readSessionCursor(page.NextCursor,r.Key,p.Expires);err==nil{t.Fatal("expired cursor accepted")}
	card:=f.g.sessionPageCard(r,"member",sessionPageText(page,false),page)
	if err:=f.g.bindQuickControls("page-card",card);err!=nil{t.Fatal(err)}
	a:=core.CardAction{ChatID:r.ChatID,UserID:"member",MessageID:"wrong-message",Value:testQuickValue(t,f.g,card,"sessions")}
	if text,err:=f.g.Action(context.Background(),a);err!=nil||!strings.Contains(text,"失效"){t.Fatal("unbound page control accepted",text,err)}
	if f.starts.Load()!=0{t.Fatal("metadata search started native")}
}

func TestSessionActivityIgnoresRecoveryAndDuplicateAdmission(t *testing.T) {
	f:=newFixture(t,nil);r:=f.room();s,err:=f.s.Session(r.Key);if err!=nil{t.Fatal(err)}
	m:=f.message("catalog-input","hello","member")
	d:=core.Delivery{UUID:core.NewID(),RoomKey:r.Key,SessionID:s.ID,Message:m}
	if err:=f.g.queueInput(d,100);err!=nil{t.Fatal(err)}
	if err:=f.g.queueInput(d,900);err!=nil{t.Fatal(err)}
	if err:=f.s.Queue(d);err!=nil{t.Fatal(err)}
	var at int64
	if err:=f.s.DB.QueryRow("SELECT at FROM session_activity WHERE session=?",s.ID).Scan(&at);err!=nil||at!=100{t.Fatal("replay reordered catalogue",at,err)}
	if err:=f.s.SetNativeTitle(r.Key,s.ID,"自动标题");err!=nil{t.Fatal(err)}
	v,err:=f.g.statusView(r);if err!=nil||stringArg(v,"sessionName")!="自动标题"{t.Fatal("status lost native title",v,err)}
}
