// code.go 从上游错误正文里取业务码（供错误聚合与面板红标归类）。
package upstream

import "regexp"

// businessCodeRe 匹配错误信封里的 code，两种真实形态都要吃：
//
//	{"code":"11-128","msg":...}   ← 老形态（字符串，带横线）
//	{"code":11128,"msg":...}      ← 新形态（数字）
//
// 只取第一处：上游把真正的业务码放在外层，内层嵌套（如 data.code）是别的语义。
var businessCodeRe = regexp.MustCompile(`"code"\s*:\s*"?([0-9A-Za-z_-]{1,16})`)

// BusinessCode 返回正文里的业务码；没有就返回空串。
func BusinessCode(body string) string {
	m := businessCodeRe.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}
