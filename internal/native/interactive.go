package native

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/process"
)

// Interactive resumes an existing native session with the same launch identity
// and model profile. Its caller must hold the service lock until tree cleanup is
// confirmed. It does not expose Feishu tools, an action relay or stdio RPC.
func Interactive(ctx context.Context,c core.Config,r core.Room,s core.Session,w core.Workspace,input *os.File,out,errOut io.Writer) error {
	cmd,cleanup,err:=interactiveCommand(c,r,s,w)
	if err!=nil{return err}
	defer cleanup()
	cmd.Stdin,cmd.Stdout,cmd.Stderr=input,out,errOut
	return process.RunInteractive(ctx,cmd)
}

func interactiveCommand(c core.Config,r core.Room,s core.Session,w core.Workspace)(*exec.Cmd,func(),error){
	if s.CCID==""||s.RoomKey!=r.Key{return nil,nil,errors.New("local session requires an existing native identity in the selected room")}
	c,err:=config.ResolveSessionModel(c,s.Profile)
	if err!=nil{return nil,nil,err}
	p,ok:=c.Project(r.ProjectID);if !ok{return nil,nil,errors.New("unknown conversation project")}
	token:=os.Getenv(c.Model.TokenEnv)
	if token==""||c.Model.BaseURL==""{return nil,nil,fmt.Errorf("missing model credential or endpoint")}
	headers,err:=config.ModelHeaders(c);if err!=nil{return nil,nil,err}
	modelEnv:=modelEnvironment(c,token,headers)
	settings,err:=privateSettings(w,modelEnv);if err!=nil{return nil,nil,err}
	cleanup:=func(){if settings!=""{_ = os.Remove(settings)}}
	env:=Environment(w,c.AppSecretEnv)
	// Terminal capabilities are needed by the local TUI, not by SDK workers.
	for _,key:=range []string{"TERM","COLORTERM","NO_COLOR"}{if value,ok:=os.LookupEnv(key);ok{env[key]=value}}
	for k,v:=range modelEnv{env[k]=v};delete(env,"ANTHROPIC_API_KEY")
	args:=[]string{"--resume",s.CCID}
	if c.Model.Model!=""{args=append(args,"--model",c.Model.Model)}
	mode:=p.PermissionMode;if mode==""{mode="acceptEdits"}
	args=append(args,"--permission-mode",mode)
	if mode=="bypassPermissions"{args=append(args,"--allow-dangerously-skip-permissions")}
	sources:="project";if w.NativeConfig=="inherit"{sources="user,project,local"}
	args=append(args,"--setting-sources",sources)
	allowed:=append([]string{"Read","Grep","Glob"},p.AllowedTools...)
	args=append(args,"--allowedTools",strings.Join(allowed,","))
	if settings!=""{args=append(args,"--settings",settings)}
	executable:=c.Runtime.ClaudeExecutable;if executable==""{executable="claude"}
	cmd:=exec.Command(executable,args...);cmd.Dir=w.Work;cmd.Env=environmentList(env)
	return cmd,cleanup,nil
}
