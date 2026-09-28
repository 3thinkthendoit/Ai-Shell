package audit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"ai-shell/internal/vault"
)

// ---- 辅助 ----

// newLoggerInDir 在指定目录里开一个带自定义轮转配置的审计日志。
// 重复调用同一目录会得到同一个主密钥（AISHELL_KEYFILE=1 让密钥落在本地文件），
// 因此可以模拟「进程重启」。
func newLoggerInDir(t *testing.T, dir string, c Config) *Logger {
	t.Helper()
	t.Setenv("AISHELL_KEYFILE", "1")
	v := vault.New(dir)
	if err := v.Open(); err != nil {
		t.Fatalf("打开凭证库失败: %v", err)
	}
	l, err := NewWithConfig(dir, v, c)
	if err != nil {
		t.Fatalf("创建审计日志失败: %v", err)
	}
	return l
}

// appendN 追加 n 条可辨识的记录。
func appendN(t *testing.T, l *Logger, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := l.Append(Entry{
			Kind: KindTool, Tool: "run_command",
			Command: fmt.Sprintf("echo n%d", i), Note: fmt.Sprintf("n%d", i),
		}); err != nil {
			t.Fatalf("追加第 %d 条失败: %v", i, err)
		}
	}
}

// archivedSegments 返回目录里的归档段文件名（已排序，不含正在写入的 audit.log）。
func archivedSegments(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, de := range des {
		if strings.HasPrefix(de.Name(), segPrefix) && strings.HasSuffix(de.Name(), segSuffix) {
			out = append(out, de.Name())
		}
	}
	sort.Strings(out)
	return out
}

// 每写一条就轮转一次的极端配置：让「跨段」这件事在几行代码内被充分触发。
// MaxFiles=-1 关掉保留策略，避免丢弃干扰链完整性断言。
var rotateEveryEntry = Config{MaxSize: 1, MaxFiles: -1}

// failSealer 包装一个真实 Sealer，在第 failAt 次 Seal 调用时返回错误，
// 用来制造「加密失败」这一**写入前置失败**，且完全不触碰磁盘上已有的记录。
//
// 为什么需要它：要断言「失败留下的序号空洞可被 Verify 发现」，就必须让空洞**两侧**
// 的记录都留在磁盘上（序号 1 在、序号 2 缺、序号 3 在）。而 Verify 的连续性校验
// 是相对「当前保留的最早一条」做的 —— 一旦把第一条也弄没了，缺失起点就退化成
// 前缀截断，与保留策略无法区分，测试也就测不到东西了。
// 早先的写法用「把日志文件换成同名目录」来制造失败，正是踩了这个坑。
type failSealer struct {
	inner  Sealer
	calls  atomic.Int32
	failAt atomic.Int32 // <=0 表示不再失败（calls 从 1 起，故永不相等）
}

func (s *failSealer) Seal(plain []byte) ([]byte, error) {
	if n := s.calls.Add(1); n == s.failAt.Load() {
		return nil, errors.New("模拟加密失败")
	}
	return s.inner.Seal(plain)
}

// Unseal 直接透传，保证 Verify / ReadAll 仍能正常解密。
func (s *failSealer) Unseal(blob []byte) ([]byte, error) { return s.inner.Unseal(blob) }

// stopFailing 让后续 Seal 恢复正常。
func (s *failSealer) stopFailing() { s.failAt.Store(0) }

// ---- 轮转本身 ----

func TestRotationArchivesSegments(t *testing.T) {
	dir := t.TempDir()
	l := newLoggerInDir(t, dir, rotateEveryEntry)
	appendN(t, l, 5)

	// 第 1 条写入 live；此后每次写入前都会把已非空的 live 归档。
	// 5 次写入 => 4 个归档段 + 1 个正在写入的段。
	segs := archivedSegments(t, dir)
	if len(segs) != 4 {
		t.Fatalf("期望 4 个归档段，实得 %d：%v", len(segs), segs)
	}
	if l.Segments() != 5 {
		t.Fatalf("期望共 5 个段（含正在写入的），实得 %d", l.Segments())
	}
	// 段名必须自解释：起止序号零填充，字典序即序号序
	if !strings.HasPrefix(segs[0], segPrefix+"0000000001-0000000001") {
		t.Fatalf("首段名不符合预期: %s", segs[0])
	}
}

func TestNoRotationBelowThreshold(t *testing.T) {
	dir := t.TempDir()
	l := newLoggerInDir(t, dir, Config{MaxSize: DefaultMaxSize, MaxFiles: -1})
	appendN(t, l, 5)

	if got := archivedSegments(t, dir); len(got) != 0 {
		t.Fatalf("未达阈值不应轮转，实得 %v", got)
	}
	if l.Segments() != 1 {
		t.Fatalf("期望只有 1 个段，实得 %d", l.Segments())
	}
}

// ---- 核心主张：链被拆到多个文件后仍然完整 ----

func TestVerifyIntactAcrossRotatedSegments(t *testing.T) {
	dir := t.TempDir()
	l := newLoggerInDir(t, dir, rotateEveryEntry)
	appendN(t, l, 20)

	if got := len(archivedSegments(t, dir)); got < 5 {
		t.Fatalf("前置条件不成立：期望发生多次轮转，实得 %d 个归档段", got)
	}

	res := l.Verify()
	if !res.OK {
		t.Fatalf("跨段哈希链应当完整，实得: %+v", res)
	}
	if res.Count != 20 {
		t.Fatalf("期望 20 条，实得 %d", res.Count)
	}
	if res.Truncated {
		t.Fatal("没有丢弃任何段，不应报告截断")
	}
	if res.StartSeq != 1 {
		t.Fatalf("起点应为序号 1，实得 %d", res.StartSeq)
	}
}

func TestReadAllAcrossSegmentsPreservesOrder(t *testing.T) {
	dir := t.TempDir()
	l := newLoggerInDir(t, dir, rotateEveryEntry)
	appendN(t, l, 15)

	all, err := l.ReadAll()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(all) != 15 {
		t.Fatalf("期望 15 条，实得 %d", len(all))
	}
	for i, e := range all {
		if e.Seq != i+1 {
			t.Fatalf("第 %d 条序号为 %d，跨段拼接后顺序错乱", i, e.Seq)
		}
		if e.Note != fmt.Sprintf("n%d", i) {
			t.Fatalf("第 %d 条内容为 %q，跨段拼接后内容错位", i, e.Note)
		}
	}
}

func TestRecentSpansSegments(t *testing.T) {
	dir := t.TempDir()
	l := newLoggerInDir(t, dir, rotateEveryEntry)
	appendN(t, l, 12)

	got, err := l.Recent(5)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("期望 5 条，实得 %d", len(got))
	}
	// 必须是最后 5 条，且时间正序
	for i, e := range got {
		if e.Seq != 8+i {
			t.Fatalf("第 %d 条序号为 %d，期望 %d（Recent 应跨段取最新）", i, e.Seq, 8+i)
		}
	}
	if got[0].Note != "n7" || got[4].Note != "n11" {
		t.Fatalf("Recent 内容错位: 首=%q 尾=%q", got[0].Note, got[4].Note)
	}
}

func TestReopenRestoresChainAcrossSegments(t *testing.T) {
	dir := t.TempDir()
	l := newLoggerInDir(t, dir, rotateEveryEntry)
	appendN(t, l, 6)
	before, _ := l.ReadAll()

	// 模拟进程重启：同一个目录、同一把密钥，重新打开
	l2 := newLoggerInDir(t, dir, rotateEveryEntry)
	if err := l2.Append(Entry{Kind: KindSystem, Note: "重启后第一条"}); err != nil {
		t.Fatalf("追加失败: %v", err)
	}

	after, err := l2.ReadAll()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(after) != 7 {
		t.Fatalf("期望 7 条，实得 %d", len(after))
	}
	// 链头必须从「最新一个非空段的最后一条」恢复，而不是从 live 段恢复
	// （live 段此刻是空的，因为重启后还没写过）。
	if after[6].PrevHash != before[5].Hash {
		t.Fatal("重启后的第一条没有接上重启前的链尾 —— 链头恢复错误")
	}
	if res := l2.Verify(); !res.OK {
		t.Fatalf("重启后校验应当通过，实得 %+v", res)
	}
}

// ---- 保留策略 ----

func TestRetentionDropsOldestSegments(t *testing.T) {
	dir := t.TempDir()
	l := newLoggerInDir(t, dir, Config{MaxSize: 1, MaxFiles: 2})
	appendN(t, l, 12)

	if got := archivedSegments(t, dir); len(got) > 2 {
		t.Fatalf("归档段应被压到 2 个，实得 %d：%v", len(got), got)
	}
	if got := archivedSegments(t, dir); len(got) == 0 {
		t.Fatal("保留策略不应把归档段清空")
	}
}

// 丢弃动作必须自己留下声明，否则「有意截断」与「被攻击者截断」无法区分。
func TestRetentionLeavesDeclarationInChain(t *testing.T) {
	dir := t.TempDir()
	l := newLoggerInDir(t, dir, Config{MaxSize: 1, MaxFiles: 2})
	appendN(t, l, 12)

	all, err := l.ReadAll()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	var found bool
	for _, e := range all {
		if e.Kind == KindRetention {
			found = true
			if !strings.Contains(e.Note, "保留策略生效") {
				t.Fatalf("截断声明的说明文案不符合预期: %q", e.Note)
			}
		}
	}
	if !found {
		t.Fatal("丢弃历史段后，链上必须留下 KindRetention 声明")
	}
}

func TestRetentionUnlimitedKeepsEverything(t *testing.T) {
	dir := t.TempDir()
	l := newLoggerInDir(t, dir, Config{MaxSize: 1, MaxFiles: -1})
	appendN(t, l, 10)

	if got := len(archivedSegments(t, dir)); got != 9 {
		t.Fatalf("MaxFiles=-1 表示不限制，期望 9 个归档段，实得 %d", got)
	}
	all, _ := l.ReadAll()
	for _, e := range all {
		if e.Kind == KindRetention {
			t.Fatal("未启用保留策略时不应出现截断声明")
		}
	}
}

// ---- 校验在轮转之后仍然能发现问题 ----

// 整段被删除：拼接后序号出现断档，必须被发现。
func TestVerifyDetectsDeletedMiddleSegment(t *testing.T) {
	dir := t.TempDir()
	l := newLoggerInDir(t, dir, rotateEveryEntry)
	appendN(t, l, 12)

	segs := archivedSegments(t, dir)
	if len(segs) < 6 {
		t.Fatalf("前置条件不成立：需要足够多的段，实得 %d", len(segs))
	}
	victim := filepath.Join(dir, segs[len(segs)/2])
	if err := os.Remove(victim); err != nil {
		t.Fatalf("删除测试段失败: %v", err)
	}

	res := l.Verify()
	if res.OK {
		t.Fatalf("整段被删除后校验必须失败，实得 %+v", res)
	}
	if !strings.Contains(res.Message, "序号不连续") {
		t.Fatalf("错误信息应指出序号不连续，实得: %s", res.Message)
	}
}

// 写入失败必须留下永久可检测的痕迹。
//
// 这是「审计日志最不该有的失败模式」的回归测试：早先 appendLocked 只在写入成功
// 后才提交 l.seq，于是失败时不推进序号 —— 序号连续、前序哈希连续、Verify 报 OK，
// 而记录实际上已经少了。用户会以为有完整轨迹。
//
// 现在失败时也会推进序号，故意在链上留一个空洞，Verify 就能报出来。
func TestAppendFailureLeavesDetectableGap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AISHELL_KEYFILE", "1")
	v := vault.New(dir)
	if err := v.Open(); err != nil {
		t.Fatalf("打开凭证库失败: %v", err)
	}

	// 第 2 次 Seal 失败 —— 即第二条记录的写入在「加密」这一步就失败。
	// 用加密失败（而非文件系统花招）是因为它不碰磁盘：第一条记录必须完好留在原地。
	s := &failSealer{inner: v}
	s.failAt.Store(2)
	l, err := NewWithConfig(dir, s, Config{MaxSize: 1 << 20, MaxFiles: -1})
	if err != nil {
		t.Fatalf("创建审计日志失败: %v", err)
	}

	if err := l.Append(Entry{Kind: KindSystem, Note: "第一条"}); err != nil {
		t.Fatalf("首条写入应成功: %v", err)
	}

	if err := l.Append(Entry{Kind: KindSystem, Note: "这条注定写不进去"}); err == nil {
		t.Fatal("加密失败时 Append 应当返回错误，而不是静默成功")
	}

	// 恢复正常，再写一条正常的。
	s.stopFailing()
	if err := l.Append(Entry{Kind: KindSystem, Note: "第三条"}); err != nil {
		t.Fatalf("恢复后写入应成功: %v", err)
	}

	// 先确认「空洞」在数据层面真实存在，再看 Verify 是否报得出来。
	ents, err := l.ReadAll()
	if err != nil {
		t.Fatalf("读取日志失败: %v", err)
	}
	if len(ents) != 2 {
		t.Fatalf("磁盘上应只有 2 条（中间那条从未落盘），实得 %d", len(ents))
	}
	if ents[0].Seq != 1 || ents[1].Seq != 3 {
		t.Fatalf("期望序号 1 与 3（跳过 2），实得 %d 与 %d", ents[0].Seq, ents[1].Seq)
	}
	// prev 只在成功时推进，所以空洞之后的记录仍接在最后一条成功记录上 —— 链不断，断的只是序号。
	if ents[1].PrevHash != ents[0].Hash {
		t.Fatal("空洞之后的记录应接在最后一条成功落盘记录的哈希上")
	}

	// 关键断言：缺失必须被发现。若失败时不推进 Seq，这里会得到 OK=true。
	res := l.Verify()
	if res.OK {
		t.Fatalf("写入失败后序号应出现空洞，Verify 必须报不完整，实得 %+v", res)
	}
	if !strings.Contains(res.Message, "序号不连续") {
		t.Fatalf("错误信息应指出序号不连续，实得: %s", res.Message)
	}
	// 断点应精确定位到第 2 条（缺的那条）。
	if res.BrokenAt != 2 {
		t.Fatalf("断点应定位到第 2 条，实得 %d", res.BrokenAt)
	}
}

// 篡改归档段里的一条记录：内容哈希不匹配，必须被发现。
func TestVerifyDetectsTamperInArchivedSegment(t *testing.T) {
	dir := t.TempDir()
	l := newLoggerInDir(t, dir, rotateEveryEntry)
	appendN(t, l, 8)

	segs := archivedSegments(t, dir)
	victim := filepath.Join(dir, segs[1])
	orig, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	// 把该段唯一一行的密文改掉（保持合法 base64，让失败发生在解密/哈希而不是解码）
	lines := strings.Split(strings.TrimSpace(string(orig)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("归档段 %s 内容为空", segs[1])
	}
	corrupted := []byte(lines[0])
	corrupted[len(corrupted)-4] ^= 0x01
	if err := os.WriteFile(victim, append(corrupted, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	res := l.Verify()
	if res.OK {
		t.Fatalf("归档段被篡改后校验必须失败，实得 %+v", res)
	}
}

// 保留策略丢弃了旧段：剩余链仍应校验通过，但必须如实报告「起点不是 1」。
func TestVerifyReportsTruncationAfterRetention(t *testing.T) {
	dir := t.TempDir()
	l := newLoggerInDir(t, dir, Config{MaxSize: 1, MaxFiles: 1})
	appendN(t, l, 15)

	res := l.Verify()
	if !res.OK {
		t.Fatalf("丢弃旧段后，剩余链本身应当完整，实得 %+v", res)
	}
	if !res.Truncated {
		t.Fatal("起点不是序号 1，必须报告 Truncated")
	}
	if res.StartSeq <= 1 {
		t.Fatalf("StartSeq 应大于 1，实得 %d", res.StartSeq)
	}
	if !strings.Contains(res.Message, "保留策略") {
		t.Fatalf("报告应提示可能与保留策略有关，实得: %s", res.Message)
	}
}

// ---- 体积与段数 ----

func TestSizeSumsAllSegments(t *testing.T) {
	dir := t.TempDir()
	l := newLoggerInDir(t, dir, rotateEveryEntry)
	appendN(t, l, 6)

	if l.Segments() != 6 {
		t.Fatalf("期望 6 个段，实得 %d", l.Segments())
	}
	// Size 是用户视角的「日志总大小」，必须涵盖归档段，否则界面会显示得越来越小
	if l.Size() <= l.LiveSize() {
		t.Fatalf("Size(%d) 应大于正在写入的段(%d)", l.Size(), l.LiveSize())
	}
}

func TestSegRangeParsesNames(t *testing.T) {
	dir := t.TempDir()
	l := newLoggerInDir(t, dir, rotateEveryEntry)
	appendN(t, l, 3)

	for _, name := range archivedSegments(t, dir) {
		first, last, err := l.segRange(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("解析段名 %s 失败: %v", name, err)
		}
		if first < 1 || last < first {
			t.Fatalf("段 %s 解析出非法区间 %d-%d", name, first, last)
		}
	}
}

// ---- 健壮性 ----

// 目录里混进无关文件（编辑器临时文件、用户误放的文件）不应让日志写入失败。
// 这要求段名匹配足够严格，而不是简单的「前缀 + 后缀」判断。
func TestStrayFilesAreIgnored(t *testing.T) {
	dir := t.TempDir()
	l := newLoggerInDir(t, dir, rotateEveryEntry)
	appendN(t, l, 3)

	strays := []string{
		"audit.seg-notanumber.log", // 非数字
		"audit.seg-1-2.log",        // 位数不足，不匹配 %010d
		"audit.log.bak",            // 前缀相同但后缀不同
		"notes.txt",
	}
	for _, n := range strays {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("garbage"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := l.Append(Entry{Kind: KindSystem, Note: "混入无关文件之后"}); err != nil {
		t.Fatalf("目录里有无关文件不应影响写入: %v", err)
	}
	if res := l.Verify(); !res.OK {
		t.Fatalf("无关文件不应影响校验: %+v", res)
	}
	// 3 次写入 => 2 个归档段 + 1 个正在写入的；第 4 次写入前再归档一次 => 4 个段。
	// 无关文件不应被计入。
	if got := l.Segments(); got != 4 {
		t.Fatalf("无关文件被误认为日志段：期望 4 个段，实得 %d", got)
	}
}

// 界面会在 agent 持续写入的同时轮询列表/校验，因此读写必须互斥。
//
// 这在 Windows 上是硬性要求：os.Rename 对「仍被打开的文件」会失败，
// 若读取期间发生轮转，归档就会失败并丢记录。Linux 上重命名打开中的文件是允许的，
// 所以这个 bug 只在 Windows 复现 —— 正是本项目的主平台。
func TestConcurrentAppendAndRead(t *testing.T) {
	dir := t.TempDir()
	// 小阈值 => 高频轮转，最大化与读取的重叠概率
	l := newLoggerInDir(t, dir, Config{MaxSize: 512, MaxFiles: -1})

	const total = 300
	var wg sync.WaitGroup
	errCh := make(chan error, 32)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < total; i++ {
			if err := l.Append(Entry{Kind: KindTool, Note: fmt.Sprintf("c%d", i)}); err != nil {
				errCh <- fmt.Errorf("并发写入失败: %w", err)
				return
			}
		}
	}()

	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				if _, err := l.Recent(20); err != nil {
					errCh <- fmt.Errorf("并发读取失败: %w", err)
					return
				}
				_ = l.Size()
				_ = l.Segments()
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	all, err := l.ReadAll()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(all) != total {
		t.Fatalf("并发写入丢了记录：期望 %d 条，实得 %d", total, len(all))
	}
	if res := l.Verify(); !res.OK {
		t.Fatalf("并发读写之后链应当完整，实得 %+v", res)
	}
}
