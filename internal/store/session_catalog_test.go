package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

func TestSessionCatalogPagedSearchAndActivity(t *testing.T) {
	s:=openTest(t)
	err:=s.Transaction(context.Background(),func(tx *sql.Tx) error {
		for i:=0;i<1000;i++ {
			id:=fmt.Sprintf("session-%04d",i)
			if _,err:=tx.Exec("INSERT INTO sessions(id,room,state,created) VALUES(?,'room','archived',?)",id,i+1);err!=nil{return err}
		}
		_,err:=tx.Exec("INSERT INTO sessions(id,room,state,created) VALUES('foreign','other','archived',2000)")
		return err
	})
	if err!=nil{t.Fatal(err)}
	seen:=map[string]bool{}
	q:=SessionQuery{Before:2000,Limit:20}
	for {
		items,more,err:=s.SearchSessions(context.Background(),"room",q)
		if err!=nil||len(items)>20{t.Fatal(items,more,err)}
		for _,item:=range items {if seen[item.ID]||item.RoomKey!="room"{t.Fatal("duplicate or foreign session",item)};seen[item.ID]=true}
		if !more{break}
		last:=items[len(items)-1];q.AfterAt,q.AfterID=last.LastInputAt,last.ID
	}
	if len(seen)!=1000{t.Fatal("pagination omitted sessions",len(seen))}
	if err:=s.SetNativeTitle("room","session-0001","修复 Login 100%_");err!=nil{t.Fatal(err)}
	if err:=s.SetNativeTitle("room","session-0002","修复 Login 100%_");err!=nil{t.Fatal(err)}
	items,_,err:=s.SearchSessions(context.Background(),"room",SessionQuery{Text:"修复 LOGIN 100%_"})
	if err!=nil||len(items)!=2{t.Fatal("literal multi-term title search",items,err)}
	if items[0].DisplayName()!="修复 Login 100%_"{t.Fatal("native title fallback missing")}
	if _,err:=s.ResolveSession("room","修复 Login 100%_");err==nil{t.Fatal("display title changed exact resume semantics")}
	if err:=s.Transaction(context.Background(),func(tx *sql.Tx) error{return RecordSessionActivity(tx,"room","session-0001",3000)});err!=nil{t.Fatal(err)}
	if err:=s.Transaction(context.Background(),func(tx *sql.Tx) error{return RecordSessionActivity(tx,"room","session-0001",2500)});err!=nil{t.Fatal(err)}
	items,_,err=s.SearchSessions(context.Background(),"room",SessionQuery{})
	if err!=nil||items[0].ID!="session-0001"||items[0].LastInputAt!=3000{t.Fatal("activity must be monotonic",items,err)}
	if err:=s.Transaction(context.Background(),func(tx *sql.Tx) error{return RecordSessionActivity(tx,"other","session-0001",5000)});err==nil{t.Fatal("cross-room activity accepted")}
	ctx,cancel:=context.WithCancel(context.Background());cancel()
	if _,_,err:=s.SearchSessions(ctx,"room",SessionQuery{});!errors.Is(err,context.Canceled){t.Fatal("search ignored cancellation",err)}
}
