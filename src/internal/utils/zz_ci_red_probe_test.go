package utils

// 临时探针：仅用于 #333 验证「坏代码 ⇒ CI 变红 ⇒ 还原 ⇒ 变绿」，验证后立即删除。
// 不要把它当成真实判据。
import "testing"

func TestCIRedProbeIntentionalFailure(t *testing.T) {
	t.Fatal("intentional CI red probe (#333): this failure is expected, the commit is reverted right after")
}
