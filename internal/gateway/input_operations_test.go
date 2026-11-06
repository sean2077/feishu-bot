package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/store"
)

func operationQueued(t *testing.T,f *fixture,id string) core.Delivery {
	t.Helper()
	r:=f.room();s,err:=f.s.Session(r.Key);if err!=nil{t.Fatal(err)}
	m:=f.message(id,"test input","member")
	if _,err=f.s.Insert(r.Key,m,"queued");err!=nil{t.Fatal(err)}
	d:=core.Delivery{UUID:core.NewID(),RoomKey:r.Key,SessionID:s.ID,Message:m}
	if err=f.s.Queue(d);err!=nil{t.Fatal(err)}
	return d
}

func TestInputCancellationExcludesConcurrentDispatch(t *testing.T) {
	f:=newFixture(t,nil);d:=operationQueued(t,f,"racing")
	unlock:=f.g.dispatchLocks.lock(f.room().Key)
	ctx,cancel:=context.WithTimeout(context.Background(),25*time.Millisecond);defer cancel()
	_,err:=f.g.cancelInput(ctx,f.room(),d.UUID,"member")
	if !errors.Is(err,context.DeadlineExceeded){unlock();t.Fatal("queued row was called safely cancelled while dispatch owned it",err)}
	if err=f.s.Mark(d.UUID,"sent");err!=nil{unlock();t.Fatal(err)}
	unlock()
	if _,err=f.g.cancelInput(context.Background(),f.room(),d.UUID,"member");!errors.Is(err,store.ErrInputHandedOff){t.Fatal(err)}
	if f.status(d.Message.ID)!="sent"{t.Fatal("submitted input altered")}
}

func TestInputDetailsAndCancellationDoNotStartOrStopNative(t *testing.T) {
	f:=newFixture(t,nil);d:=operationQueued(t,f,"cancel-own")
	text,err:=f.g.inputsView(context.Background(),f.room(),"member",true)
	if err!=nil||!strings.Contains(text,d.UUID)||!strings.Contains(text,"cancelRequiresDispatchCheck")||!strings.Contains(text,"member"){t.Fatal(text,err)}
	if _,err=f.g.cancelInput(context.Background(),f.room(),d.UUID,"second");!errors.Is(err,store.ErrInputNotOwned){t.Fatal(err)}
	if _,err=f.g.cancelInput(context.Background(),f.room(),d.UUID,"member");err!=nil{t.Fatal(err)}
	if f.status(d.Message.ID)!="cancelled"||f.starts.Load()!=0||f.tr.receiptDone(d.Message.ID){t.Fatal("input cancellation had native/completion effects")}
}

func TestInputOperationCommandIsReadOnlyAndValidatesArguments(t *testing.T) {
	f:=newFixture(t,nil)
	for _,cmd:=range []string{"/inputs","/inputs --json","/inputs ignored","/cancel-input"}{
		handled,err:=f.g.operationCommand(context.Background(),f.room(),f.message("command",cmd,"member"));if err!=nil||!handled{t.Fatal(cmd,err)}
	}
	if f.starts.Load()!=0{t.Fatal("management command started native worker")}
}
