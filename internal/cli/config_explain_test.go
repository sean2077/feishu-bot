package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
)

func TestConfigExplainUsesRuntimeBindingAndRolePolicy(t *testing.T) {
	c:=core.Config{TenantID:"tenant",AppID:"app",Projects:[]core.Project{{ID:"explicit",Maintainers:[]string{"owner"}},{ID:"fallback",Maintainers:[]string{"admin"}}},Groups:[]core.Group{{ChatID:"*",ProjectID:"fallback",AllowAllMembers:true,MentionOptional:true},{ChatID:"group",ProjectID:"explicit",Members:[]string{"member"}}},DMs:[]core.DM{{UserID:"person",ProjectID:"explicit"}}}
	for _,tc:=range []struct{chat,user,kind string;mention,bot bool;binding,reason string;manage,approve bool}{
		{"group","member","group",false,false,"explicit_group","mention_required",false,false},
		{"group","owner","group",true,false,"explicit_group","admitted",true,true},
		{"group","outsider","group",true,false,"explicit_group","author_not_allowed",false,false},
		{"unknown","person","group",false,false,"default_group","admitted",false,false},
		{"dm","person","p2p",false,false,"dm","admitted",true,false},
		{"dm","outsider","p2p",true,false,"none","no_binding",false,false},
		{"group","owner","group",true,true,"explicit_group","bot_sender",false,false},
	} {
		v:=explainRoute(c,Options{ExplainChat:tc.chat,ExplainUser:tc.user,ChatType:tc.kind,Mentioned:tc.mention,SenderBot:tc.bot})
		if v["binding"]!=tc.binding||v["reason"]!=tc.reason||v["canManageSession"]!=tc.manage||v["canApproveTools"]!=tc.approve{t.Fatal(tc,v)}
	}
}

func TestConfigExplainIsOfflineAndReadOnly(t *testing.T) {
	dir:=t.TempDir();path:=filepath.Join(dir,"config.json")
	if err:=os.WriteFile(path,[]byte(config.Example),0600);err!=nil{t.Fatal(err)}
	o,err:=Parse([]string{"config","explain","--config",path,"--chat","unbound","--user","user","--json"});if err!=nil{t.Fatal(err)}
	var out bytes.Buffer
	if err:=runConfigExplain(context.Background(),o,&out);err!=nil{t.Fatal(err)}
	var v core.Args;if err:=json.Unmarshal(out.Bytes(),&v);err!=nil{t.Fatal(err)}
	after,_:=os.ReadFile(path);if string(after)!=config.Example{t.Fatal("config explain rewrote config")}
	entries,err:=os.ReadDir(dir);if err!=nil||len(entries)!=1{t.Fatal("config explain created state",entries,err)}
	if strings.Contains(out.String(),"baseUrl")||v["scope"]!="offline_config_policy"{t.Fatal("unexpected credentials or scope",out.String())}
	for _,args:=range [][]string{{"config","explain"},{"config","explain","--chat","g","--user","u","--chat-type","invalid"},{"config","explain","--chat","g","--chat","g","--user","u"}}{if _,err:=Parse(args);err==nil{t.Fatal("invalid explain options accepted",args)}}
}
