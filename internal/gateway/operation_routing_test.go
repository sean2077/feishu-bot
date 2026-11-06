package gateway

import "testing"

func TestCancelInputCommandReleasesAdmissionLockBeforeOperation(t *testing.T) {
	f:=newFixture(t,nil)
	d:=operationQueued(t,f,"cancel-through-message")
	f.send(f.message("cancel-command","/cancel-input "+d.UUID,"member"))
	eventually(t,func()bool{return f.status(d.Message.ID)=="cancelled"})
	if f.starts.Load()!=0||f.tr.receiptDone(d.Message.ID){t.Fatal("cancel command invoked native or confirmed execution")}
}
