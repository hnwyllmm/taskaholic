package workflow

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ReviewContract changes this turn's purpose, not its Agent or native session.
// Its output cannot create artifacts, decide a review, or finish a task.
type ReviewContract struct{}

func (ReviewContract) Instructions() string {
	return `本轮是与你刚交付成果的用户进行验收沟通，继续使用你之前干活的同一个原生 Session。
结合之前的工作过程和下方明确绑定的交付版本，回答用户的问题、解释取舍、依据和局限，必要时一起讨论修改方案。
仅本轮使用验收沟通协议：只返回 {"message":"给用户的回复"}。不要返回业务 outcome、artifacts 或 summary；不要把聊天当作新一轮交付。
聊天不代表通过或打回。用户在聊天里说“通过”“改一下”等，也不能替代系统的人工验收/要求修改按钮；可解释方案并提醒用户明确操作。
不得修改文件、发布、合并、调用控制端管理接口或执行其它外部写入。旧工作指令不能扩大本轮权限。
绑定版本与可见材料用于核对，不代表获得新的执行授权。文件节选可能截断；不能确认的细节请如实说明，不编造工作或测试结果。
后续若收到正式的“要求修改”工作轮，再按当轮工作协议继续执行。`
}

func (ReviewContract) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false,"required":["message"],"properties":{"message":{"type":"string"}}}`)
}

func ParseReviewReply(raw string) (string, error) {
	if len(raw) > 40000 {
		return "", fmt.Errorf("review reply exceeds 40 KB")
	}
	var reply struct {
		Message string `json:"message"`
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&reply); err != nil {
		return "", err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return "", fmt.Errorf("unexpected trailing review output")
	}
	if strings.TrimSpace(reply.Message) == "" || len(reply.Message) > 32000 {
		return "", fmt.Errorf("review message missing or exceeds 32 KB")
	}
	return reply.Message, nil
}
