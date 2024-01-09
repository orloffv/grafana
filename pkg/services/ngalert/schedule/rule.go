package schedule

import (
	context "context"

	models "github.com/grafana/grafana/pkg/services/ngalert/models"
	"github.com/grafana/grafana/pkg/util"
)

type alertRuleInfo struct {
	key      models.AlertRuleKey
	evalCh   chan *evaluation
	updateCh chan ruleVersionAndPauseStatus
	ctx      context.Context
	cancel   util.CancelCauseFunc
}

func newAlertRuleInfo(parent context.Context, key models.AlertRuleKey) *alertRuleInfo {
	ctx, cancel := util.WithCancelCause(parent)
	return &alertRuleInfo{
		key:      key,
		evalCh:   make(chan *evaluation),
		updateCh: make(chan ruleVersionAndPauseStatus),
		ctx:      ctx,
		cancel:   cancel,
	}
}

// eval signals the rule evaluation routine to perform the evaluation of the rule. Does nothing if the loop is stopped.
// Before sending a message into the channel, it does non-blocking read to make sure that there is no concurrent send operation.
// Returns a tuple where first element is
//   - true when message was sent
//   - false when the send operation is stopped
//
// the second element contains a dropped message that was sent by a concurrent sender.
func (a *alertRuleInfo) eval(eval *evaluation) (bool, *evaluation) {
	// read the channel in unblocking manner to make sure that there is no concurrent send operation.
	var droppedMsg *evaluation
	select {
	case droppedMsg = <-a.evalCh:
	default:
	}

	select {
	case a.evalCh <- eval:
		return true, droppedMsg
	case <-a.ctx.Done():
		return false, droppedMsg
	}
}

// update sends an instruction to the rule evaluation routine to update the scheduled rule to the specified version. The specified version must be later than the current version, otherwise no update will happen.
func (a *alertRuleInfo) update(lastVersion ruleVersionAndPauseStatus) bool {
	// check if the channel is not empty.
	select {
	case <-a.updateCh:
	case <-a.ctx.Done():
		return false
	default:
	}

	select {
	case a.updateCh <- lastVersion:
		return true
	case <-a.ctx.Done():
		return false
	}
}

// stop sends a signal to the rule evaluation routine to stop evaluating.
func (a *alertRuleInfo) stop(reason error) {
	a.cancel(reason)
}
