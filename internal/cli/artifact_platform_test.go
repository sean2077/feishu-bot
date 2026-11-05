package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
	"github.com/sean2077/feishu-bot/internal/core"
)

type artifactCard struct { ID,ReplyTo,CardID string; Body core.Args }
type artifactWire struct { conn *websocket.Conn; mu sync.Mutex }
func(w *artifactWire)write(frame larkws.Frame)error{
	data,err:=frame.Marshal();if err!=nil{return err}
	w.mu.Lock();defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(5*time.Second))
	return w.conn.WriteMessage(websocket.BinaryMessage,data)
}
type artifactPlatform struct {
	t *testing.T
	mu sync.Mutex
	changed chan struct{}
	connections chan *artifactWire
	cards map[string]artifactCard
	sequences map[string]int64
	acks map[string]larkws.Response
	done map[string]bool
	fileSeen chan string
	fileRelease chan struct{}
	releaseOnce sync.Once
	counter atomic.Int64
	fileMessages int
}
func newArtifactPlatform(t *testing.T)*artifactPlatform{
	return &artifactPlatform{t:t,changed:make(chan struct{},1),connections:make(chan *artifactWire,8),cards:map[string]artifactCard{},sequences:map[string]int64{},acks:map[string]larkws.Response{},done:map[string]bool{},fileSeen:make(chan string,2),fileRelease:make(chan struct{})}
}
func(f *artifactPlatform)signal(){select{case f.changed<-struct{}{}:default:}}
func(f *artifactPlatform)releaseFile(){f.releaseOnce.Do(func(){close(f.fileRelease)})}
func artifactJSON(w http.ResponseWriter,data any){w.Header().Set("Content-Type","application/json");_ = json.NewEncoder(w).Encode(core.Args{"code":0,"data":data})}
func artifactBody(t *testing.T,r *http.Request)core.Args{
	t.Helper();var body core.Args
	if json.NewDecoder(io.LimitReader(r.Body,2<<20)).Decode(&body)!=nil{t.Error("artifact sent invalid JSON")}
	return body
}
func artifactString(body core.Args,key string)string{value,_:=body[key].(string);return value}
func decodeArtifactCard(value any)core.Args{v,_:=value.(string);var card core.Args;_ = json.Unmarshal([]byte(v),&card);return card}
func(f *artifactPlatform)serveHTTP(w http.ResponseWriter,r *http.Request){
	path:=r.URL.Path
	if path=="/open-apis/auth/v3/tenant_access_token/internal"{
		w.Header().Set("Content-Type","application/json");_,_=io.WriteString(w,`{"code":0,"tenant_access_token":"artifact-token","expire":7200}`);return
	}
	if path=="/open-apis/bot/v3/info"{artifactJSON(w,core.Args{"open_id":"ou_bot"});return}
	if path=="/callback/ws/endpoint"{
		artifactJSON(w,core.Args{"URL":"wss://open.feishu.cn/artifact-ws?service_id=3&device_id=artifact","ClientConfig":core.Args{"PingInterval":1,"ReconnectInterval":1}});return
	}
	if path=="/artifact-ws"{f.websocket(w,r);return}
	if r.Header.Get("Authorization")!="Bearer artifact-token"{http.Error(w,"fixture authorization",403);return}
	if path=="/open-apis/cardkit/v1/cards"&&r.Method==http.MethodPost{
		body:=artifactBody(f.t,r)
		if decodeArtifactCard(body["data"])==nil{f.t.Error("invalid CardKit create payload")}
		artifactJSON(w,core.Args{"card_id":fmt.Sprintf("card_%d",f.counter.Add(1))});return
	}
	if strings.HasPrefix(path,"/open-apis/cardkit/v1/cards/")&&r.Method==http.MethodPut{
		id:=strings.Split(strings.TrimPrefix(path,"/open-apis/cardkit/v1/cards/"),"/")[0]
		body:=artifactBody(f.t,r);sequence,_:=body["sequence"].(float64)
		f.mu.Lock();if int64(sequence)<=f.sequences[id]{f.t.Error("artifact reused a card sequence")};f.sequences[id]=int64(sequence);f.mu.Unlock()
		artifactJSON(w,core.Args{});return
	}
	if path=="/open-apis/im/v1/files"&&r.Method==http.MethodPost{
		reader,err:=r.MultipartReader();if err!=nil{f.t.Error(err);http.Error(w,"multipart required",400);return}
		var file []byte
		for{
			part,err:=reader.NextPart();if err==io.EOF{break};if err!=nil{f.t.Error(err);break}
			if part.FormName()=="file"{file,err=io.ReadAll(io.LimitReader(part,2<<20));if err!=nil{f.t.Error(err)}}
			_ = part.Close()
		}
		select{case f.fileSeen<-string(file):case<-r.Context().Done():return}
		select{case<-f.fileRelease:case<-r.Context().Done():return}
		artifactJSON(w,core.Args{"file_key":"file_artifact_complete"});return
	}
	if path=="/open-apis/im/v1/messages"&&r.Method==http.MethodGet{
		artifactJSON(w,core.Args{"items":[]core.Args{{"message_id":"om_history","chat_id":"oc_artifact","msg_type":"text","create_time":strconv.FormatInt(time.Now().Add(-time.Minute).UnixMilli(),10),"sender":core.Args{"id":"ou_member","id_type":"open_id","sender_type":"user"},"body":core.Args{"content":`{"text":"ARTIFACT_HISTORY_MARKER"}`}}},"has_more":false});return
	}
	prefix:="/open-apis/im/v1/messages"
	if strings.HasPrefix(path,prefix){
		parts:=strings.Split(strings.Trim(strings.TrimPrefix(path,prefix),"/"),"/")
		if len(parts)>=2&&parts[1]=="reactions"{
			if r.Method==http.MethodPost{
				body:=artifactBody(f.t,r);reaction,_:=body["reaction_type"].(map[string]any)
				if reaction["emoji_type"]=="Done"{f.mu.Lock();f.done[parts[0]]=true;f.mu.Unlock();f.signal()}
				artifactJSON(w,core.Args{"reaction_id":fmt.Sprintf("reaction_%d",f.counter.Add(1))});return
			}
			artifactJSON(w,core.Args{"items":[]any{},"has_more":false});return
		}
		if r.Method==http.MethodPatch&&len(parts)==1{
			body:=artifactBody(f.t,r)
			f.mu.Lock();old:=f.cards[parts[0]];old.Body=decodeArtifactCard(body["content"]);f.cards[parts[0]]=old;f.mu.Unlock();f.signal();artifactJSON(w,core.Args{});return
		}
		if r.Method==http.MethodPost&&(path==prefix||(len(parts)==2&&parts[1]=="reply")){
			body:=artifactBody(f.t,r);id:=fmt.Sprintf("om_output_%d",f.counter.Add(1))
			replyTo:="";if len(parts)==2{replyTo=parts[0]}
			content:=decodeArtifactCard(body["content"])
			f.mu.Lock()
			if body["msg_type"]=="interactive"{
				cardID:="";if content["type"]=="card"{ref,_:=content["data"].(map[string]any);cardID=artifactString(ref,"card_id")}
				f.cards[id]=artifactCard{ID:id,ReplyTo:replyTo,CardID:cardID,Body:content}
			}
			if body["msg_type"]=="file"{f.fileMessages++}
			f.mu.Unlock();f.signal();artifactJSON(w,core.Args{"message_id":id});return
		}
	}
	f.t.Errorf("unexpected artifact Feishu endpoint: %s %s",r.Method,path)
	http.Error(w,"fixture endpoint only",404)
}

func(f *artifactPlatform)websocket(w http.ResponseWriter,r *http.Request){
	conn,err:=(&websocket.Upgrader{}).Upgrade(w,r,nil);if err!=nil{f.t.Error("artifact websocket upgrade",err);return}
	defer conn.Close();wire:=&artifactWire{conn:conn};f.connections<-wire
	for{
		_,data,err:=conn.ReadMessage();if err!=nil{return}
		var frame larkws.Frame
		if frame.Unmarshal(data)!=nil{f.t.Error("invalid artifact websocket frame");return}
		if larkws.FrameType(frame.Method)==larkws.FrameTypeControl{
			for i:=range frame.Headers{if frame.Headers[i].Key==larkws.HeaderType{frame.Headers[i].Value=string(larkws.MessageTypePong)}}
			frame.Payload=nil;_ = wire.write(frame);continue
		}
		id:=larkws.Headers(frame.Headers).GetString(larkws.HeaderMessageID)
		var response larkws.Response
		if json.Unmarshal(frame.Payload,&response)!=nil{f.t.Error("invalid artifact event acknowledgement");return}
		f.mu.Lock();f.acks[id]=response;f.mu.Unlock();f.signal()
	}
}
func artifactButton(value any,label string)core.Args{
	switch v:=value.(type){
	case map[string]any:
		text,_:=v["text"].(map[string]any)
		if v["tag"]=="button"&&text["content"]==label{result,_:=v["value"].(map[string]any);return result}
		for _,child:=range v{if result:=artifactButton(child,label);result!=nil{return result}}
	case []any:for _,child:=range v{if result:=artifactButton(child,label);result!=nil{return result}}
	}
	return nil
}
func(f *artifactPlatform)wait(t *testing.T,ctx context.Context,what string,condition func()bool){
	t.Helper();for{if condition(){return};select{case<-f.changed:case<-ctx.Done():t.Fatal("artifact journey timed out: "+what)}}
}
func(f *artifactPlatform)button(t *testing.T,ctx context.Context,label string)(artifactCard,core.Args){
	t.Helper();var found artifactCard;var value core.Args
	f.wait(t,ctx,"card button "+label,func()bool{
		f.mu.Lock();defer f.mu.Unlock()
		for _,card:=range f.cards{
			if v:=artifactButton(card.Body,label);v!=nil{found=card;data,_:=json.Marshal(v);_ = json.Unmarshal(data,&value);return true}
		}
		return false
	});return found,value
}
func(f *artifactPlatform)connected(t *testing.T,ctx context.Context)*artifactWire{
	t.Helper();select{case wire:=<-f.connections:return wire;case<-ctx.Done():t.Fatal("artifact did not establish the expected WS connection");return nil}
}
func(f *artifactPlatform)event(t *testing.T,ctx context.Context,wire *artifactWire,payload core.Args)larkws.Response{
	t.Helper();id:=core.NewID();data,_:=json.Marshal(payload)
	frame:=larkws.Frame{SeqID:1,LogID:2,Method:int32(larkws.FrameTypeData),Service:3,Headers:[]larkws.Header{{Key:larkws.HeaderType,Value:string(larkws.MessageTypeEvent)},{Key:larkws.HeaderMessageID,Value:id},{Key:larkws.HeaderSum,Value:"1"},{Key:larkws.HeaderSeq,Value:"0"}},Payload:data}
	if err:=wire.write(frame);err!=nil{t.Fatal("send local artifact event",err)}
	var response larkws.Response
	f.wait(t,ctx,"event acknowledgement",func()bool{f.mu.Lock();defer f.mu.Unlock();var ok bool;response,ok=f.acks[id];return ok})
	if response.StatusCode!=200{t.Fatalf("artifact event failed: status=%d",response.StatusCode)}
	return response
}
func(f *artifactPlatform)message(t *testing.T,ctx context.Context,wire *artifactWire,id,text,author string){
	t.Helper();content,_:=json.Marshal(core.Args{"text":"@_user_1 "+text})
	f.event(t,ctx,wire,core.Args{"schema":"2.0","header":core.Args{"event_type":"im.message.receive_v1","event_id":core.NewID()},"event":core.Args{"sender":core.Args{"sender_id":core.Args{"open_id":author},"sender_type":"user"},"message":core.Args{"message_id":id,"chat_id":"oc_artifact","chat_type":"group","message_type":"text","content":string(content),"create_time":strconv.FormatInt(time.Now().UnixMilli(),10),"mentions":[]core.Args{{"key":"@_user_1","id":core.Args{"open_id":"ou_bot"},"name":"Fixture"}}}}})
}
func(f *artifactPlatform)action(t *testing.T,ctx context.Context,wire *artifactWire,card,user string,value core.Args)string{
	t.Helper();response:=f.event(t,ctx,wire,core.Args{"schema":"2.0","header":core.Args{"event_type":"card.action.trigger"},"event":core.Args{"operator":core.Args{"open_id":user},"context":core.Args{"open_chat_id":"oc_artifact","open_message_id":card},"action":core.Args{"value":value}}})
	var body struct{Toast struct{Content string}};_ = json.Unmarshal(response.Data,&body);return body.Toast.Content
}
func(f *artifactPlatform)isDone(id string)bool{f.mu.Lock();defer f.mu.Unlock();return f.done[id]}
