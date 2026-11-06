package gateway

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRetryScanAdvancesPastNotYetDueCandidates(t *testing.T) {
	f:=newFixture(t,nil);r:=f.room()
	future:=time.Now().Add(time.Hour).UnixMilli()
	if err:=f.s.Transaction(context.Background(),func(tx *sql.Tx)error{
		for i:=0;i<retryCandidateLimit+1;i++{
			due:=future;if i==retryCandidateLimit{due=0}
			if _,err:=tx.Exec(`INSERT INTO outbox(id,room,session,text,success,state,at,retry_after) VALUES(?,?,'s','saved',1,'pending',?,?)`,fmt.Sprintf("candidate-%03d",i),r.Key,i+1,due);err!=nil{return err}
		}
		return nil
	});err!=nil{t.Fatal(err)}
	if ids:=f.g.retryable(r.Key);len(ids)!=0{t.Fatal("candidate budget bypassed",ids)}
	ids:=f.g.retryable(r.Key)
	if len(ids)!=1||ids[0]!=fmt.Sprintf("candidate-%03d",retryCandidateLimit){t.Fatal("not-yet-due prefix starved later output",ids)}
}

func TestRetryCandidatePlanAvoidsSortingRetryAfterRange(t *testing.T) {
	f:=newFixture(t,nil)
	rows,err:=f.s.DB.Query("EXPLAIN QUERY PLAN "+retryCandidateQuery+" AND (o.at,o.id)>(?,?) ORDER BY o.at,o.id LIMIT ?",f.room().Key,0,"",retryCandidateLimit)
	if err!=nil{t.Fatal(err)};defer rows.Close()
	var plan strings.Builder
	for rows.Next(){var a,b,c int;var detail string;if err=rows.Scan(&a,&b,&c,&detail);err!=nil{t.Fatal(err)};plan.WriteString(detail)}
	if err=rows.Err();err!=nil{t.Fatal(err)}
	if !strings.Contains(plan.String(),"outbox_room_state_age")||strings.Contains(plan.String(),"TEMP B-TREE"){t.Fatal("retry candidate query lost its ordered range index",plan.String())}
}
