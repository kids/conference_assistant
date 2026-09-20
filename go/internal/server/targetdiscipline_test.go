package server

import (
	"strings"
	"testing"
)

// TestSetTargetDisciplineValidation 目标学科会拼进每一次 AI 调用的上下文
// （含报告总结），必须挡住误输入/自动填充的异常值：
// 空值回默认；2~20 字；无换行制表符；校验失败时不得覆盖当前值。
func TestSetTargetDisciplineValidation(t *testing.T) {
	rt := &Runtime{}

	if v, err := rt.SetTargetDiscipline("结构生物学"); err != nil || v != "结构生物学" {
		t.Fatalf("合法值应生效，实际 %q err=%v", v, err)
	}
	if v, err := rt.SetTargetDiscipline(""); err != nil || v != DefaultTargetDiscipline {
		t.Errorf("空值应回默认，实际 %q err=%v", v, err)
	}

	// 以下都应被拒绝，且当前值保持不变
	rt.SetTargetDiscipline("材料科学")
	for _, bad := range []string{"黑", strings.Repeat("长", 21), "带\n换行", "带\t制表"} {
		v, err := rt.SetTargetDiscipline(bad)
		if err == nil {
			t.Errorf("%q 应被拒绝，实际被接受", bad)
		}
		if v != "材料科学" {
			t.Errorf("拒绝后当前值应保持「材料科学」，实际 %q", v)
		}
	}
}
