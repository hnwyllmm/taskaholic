package model

// A system slot defines a job, not a privileged Agent identity. Its executor
// is a normal team member; permissions and output contracts belong to the job.
type SystemSlot struct {
	ID          string `json:"slot"`
	Name        string `json:"name"`
	Description string `json:"description"`
	DefaultMode string `json:"default_mode"`
}

var SystemSlots = []SystemSlot{
	{"home_chat", "首页聊天", "新对话使用指定成员；已有对话继续原成员和 Session。", "auto"},
	{"task_consultation", "任务旁路咨询", "只读解释任务已记录的信息；使用独立 Session，不打断或指导工作成员。", "auto"},
	{"role_builder", "角色设计", "为新角色草案生成职责和能力配置；已有草案继续原 Session。", "auto"},
	{"task_router", "任务路由", "新任务可由指定成员在合格候选人中选择执行者；已有任务保持归属。", "rules"},
	{"upgrade_builder", "升级构建", "使用本机成员在隔离副本中修改并测试系统；安装仍需人工确认。", "auto"},
}

func FindSystemSlot(id string) (SystemSlot, bool) {
	for _, slot := range SystemSlots {
		if slot.ID == id {
			return slot, true
		}
	}
	return SystemSlot{}, false
}

type SystemBinding struct {
	Slot        string `json:"slot"`
	Mode        string `json:"mode"`
	AgentID     string `json:"agent_id,omitempty"`
	Version     int64  `json:"version"`
	UpdatedAtMS int64  `json:"updated_at_ms"`
}

// Durable decisions survive polling and process restarts. An AI suggestion
// never grants permission: assignment validates the candidate again in a Tx.
type RoutingDecision struct {
	ID             string        `json:"decision_id"`
	TaskID         string        `json:"task_id"`
	TaskVersion    int64         `json:"task_version"`
	InternalTaskID string        `json:"internal_task_id"`
	RunID          string        `json:"run_id"`
	Binding        SystemBinding `json:"binding"`
	Router         AgentProfile  `json:"router"`
	Candidates     []string      `json:"candidate_ids"`
	State          string        `json:"state"`
	AgentID        string        `json:"agent_id,omitempty"`
	Reason         string        `json:"reason,omitempty"`
	Error          string        `json:"error,omitempty"`
	CreatedAtMS    int64         `json:"created_at_ms"`
}
