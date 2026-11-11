package gateway

import "github.com/sean2077/feishu-bot/internal/core"

const selfApprovalToast = "不能审批自己发起的请求"
const unknownRequesterToast = "无法确认发起人，仅项目 owner 可以批准"

// approvalPolicy captures the approver set and self-approval rules for one
// pending permission request. Question cards do not use it.
type approvalPolicy struct {
	approvers          []string
	owners             []string
	forbidSelfApproval bool
	designated         bool
	requestContext     *core.RequestContext
	tainted            bool
	taintTargets       []string
}

// taintedApprovalPolicy builds the additional confirmation shown to the DM
// user after a cross-chat read. It is not a replacement for an action's normal
// approval: callers that already have an action approval must keep the original
// approvers and self-approval rule and ask again after this confirmation.
func taintedApprovalPolicy(p approvalPolicy, dm string, targets []string) approvalPolicy {
	if dm != "" {
		p.approvers = []string{dm}
		p.designated = true
		p.forbidSelfApproval = false
	}
	p.tainted = true
	p.taintTargets = append([]string(nil), targets...)
	return p
}

func approvalPolicyForAction(p core.Project, a core.ActionSpec, requestContext *core.RequestContext) approvalPolicy {
	approvers := a.Approvers
	if len(approvers) == 0 {
		approvers = p.Maintainers
	}
	return approvalPolicy{
		approvers:          approvers,
		owners:             p.Owners,
		forbidSelfApproval: p.Approvals.ForbidSelfApproval,
		designated:         len(a.Approvers) > 0,
		requestContext:     requestContext,
	}
}

func approvalPolicyForTool(p core.Project, requestContext *core.RequestContext) approvalPolicy {
	return approvalPolicy{
		approvers:          p.Maintainers,
		owners:             p.Owners,
		forbidSelfApproval: p.Approvals.ForbidSelfApproval,
		requestContext:     requestContext,
	}
}

// decisionAllowed is the single approval-qualification check used by every
// permission entry point. Denials are not restricted by the self-approval
// rule, but the user must still be an approver. The third return value names
// the security-relevant rejection reason for audit, if any.
func (p *approval) decisionAllowed(user string, deny bool) (bool, string, string) {
	if p.kind != "permission" {
		return true, "", ""
	}
	approvers := p.approvers
	if len(approvers) == 0 {
		// Programmatic approvals and older test fixtures may omit the captured
		// set; the room's configured maintainers are the default.
		approvers = p.room.Maintainers
	}
	if !contains(approvers, user) {
		if p.designated {
			return false, "仅指定审批人可以审批", ""
		}
		return false, "仅维护者可以审批", ""
	}
	if deny {
		return true, "", ""
	}
	if p.forbidSelfApproval {
		if p.requester == nil || len(p.requester.Senders) == 0 {
			if !contains(p.owners, user) {
				return false, unknownRequesterToast, "unknown_requester"
			}
		} else if contains(p.requester.Senders, user) && !contains(p.owners, user) {
			return false, selfApprovalToast, "self"
		}
	}
	return true, "", ""
}

func (p *approval) approverLabel() string {
	if p.designated {
		return "指定审批人"
	}
	return "项目维护者"
}

func requesterLine(requestContext *core.RequestContext) string {
	if requestContext != nil {
		if label := formattedRequester(requestContext); label != "" {
			return "发起人：" + label
		}
	}
	return "发起人：未知（后台）"
}
