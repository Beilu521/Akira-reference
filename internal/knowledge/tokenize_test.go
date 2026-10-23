package knowledge

import (
	"testing"
)

// TestSignatureFrozenOnUnigram 验证签名分词冻结在单字切分:
// SignatureFor 是存量卡片的去重指纹,这里锁死一个期望值 ——
// 谁要是把 SignatureFor 换成其他分词,存量库的签名全失效,这个测试负责拦住他。
// (分词方案本身的实测选型结论见 Tokenize 注释与 TestSparseRetrievalBaseline)
func TestSignatureFrozenOnUnigram(t *testing.T) {
	got := SignatureFor("docker_oom", "容器 OOMKilled 内存超限")
	// 汉字单字 + ASCII 词,排序去重:ASCII 在前,汉字按码点排序
	want := "docker_oom|oomkilled,内,器,存,容,超,限"
	if got != want {
		t.Errorf("签名分词行为发生变化(会影响存量库去重!): got %q want %q", got, want)
	}
}
