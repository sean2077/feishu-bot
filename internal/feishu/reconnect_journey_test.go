package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
	"github.com/sean2077/feishu-bot/internal/core"
)

// A successful handshake alone is insufficient: all registered message and
// interaction handlers must still be attached to the replacement connection.
// This is the real transport boundary with a local WS fixture, not live Feishu
// acceptance or a claim that the native CLI executed these commands.
func TestReconnectDispatchesMessagesAnswersAndStop(t *testing.T) {
	var origin string
	var connections atomic.Int32
	finished:=make(chan struct{})
	var finishOnce sync.Once
	upgrader:=websocket.Upgrader{}
	message:=func(id,text,author string)[]byte{
		data:=rawEvent("oc_allowed",author,"text",`{"text":"@_user_1 `+text+`"}`,true)
		var body map[string]any
		_ = json.Unmarshal(data,&body)
		body["event"].(map[string]any)["message"].(map[string]any)["message_id"]=id
		value,_:=json.Marshal(body)
		return value
	}
	client,server:=fixtureClient(t,nil,func(w http.ResponseWriter,r *http.Request){
		if r.URL.Path=="/callback/ws/endpoint"{
			jsonResponse(w,200,core.Args{"code":0,"data":core.Args{"URL":strings.Replace(origin,"http://","ws://",1)+"/ws?service_id=3","ClientConfig":core.Args{"ReconnectInterval":1,"PingInterval":1}}})
			return
		}
		conn,err:=upgrader.Upgrade(w,r,nil);if err!=nil{return}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(8*time.Second))
		first:=connections.Add(1)==1
		frames:=map[string][]byte{"before":message("om_before","hello","ou_member")}
		if !first{
			frames=map[string][]byte{
				"continued":message("om_continued","continue","ou_member"),
				"text-answer":message("om_answer","/answer question-id answer","ou_member"),
				"stop":message("om_stop","/stop","ou_owner"),
				"denied":message("om_denied","must remain rejected","ou_stranger"),
				"button-answer":[]byte(`{"schema":"2.0","header":{"event_type":"card.action.trigger"},"event":{"operator":{"open_id":"ou_owner"},"context":{"open_chat_id":"oc_allowed","open_message_id":"om_question"},"action":{"value":{"requestId":"question-id","choice":"answer"},"form_value":{"answer":"continued after reconnect"}}}}`),
			}
		}
		for id,payload:=range frames{if !writeFrame(t,conn,wireEvent(id,payload)){return}}
		pending:=len(frames)
		for{
			_,data,err:=conn.ReadMessage();if err!=nil{return}
			var frame larkws.Frame
			if frame.Unmarshal(data)!=nil{t.Error("invalid reconnect acknowledgement");return}
			id:=larkws.Headers(frame.Headers).GetString(larkws.HeaderMessageID)
			if _,ok:=frames[id];!ok{continue}
			var response larkws.Response
			if json.Unmarshal(frame.Payload,&response)!=nil||response.StatusCode!=200{t.Error("reconnected event did not acknowledge success");return}
			delete(frames,id);pending--
			if pending==0{
				if first{return}
				finishOnce.Do(func(){close(finished)})
			}
		}
	})
	origin=server.URL
	var mu sync.Mutex
	seen:=map[string]string{}
	answers:=0
	ctx,cancel:=context.WithCancel(context.Background());defer cancel()
	done:=make(chan error,1)
	go func(){done<-client.Start(ctx,func(_ context.Context,m core.Message)error{
		mu.Lock();defer mu.Unlock();seen[m.ID]=m.Text;return nil
	},func(_ context.Context,a core.CardAction)(string,error){
		mu.Lock();defer mu.Unlock()
		if a.MessageID!="om_question"||a.UserID!="ou_owner"||a.FormValue["answer"]!="continued after reconnect"{t.Error("reconnected callback lost origin or answer")}
		answers++;return "已确认",nil
	})}()
	await(t,finished,"post-reconnect message, answer and stop acknowledgements")
	cancel()
	select{case err:=<-done:if err!=nil{t.Fatal(err)};case<-time.After(2*time.Second):t.Fatal("reconnected client did not stop")}
	mu.Lock();defer mu.Unlock()
	if connections.Load()!=2||len(seen)!=4||seen["om_continued"]!="continue"||seen["om_answer"]!="/answer question-id answer"||seen["om_stop"]!="/stop"||answers!=1{t.Fatalf("lost handlers or broadened authorization after reconnect: connections=%d messages=%v answers=%d",connections.Load(),seen,answers)}
}
