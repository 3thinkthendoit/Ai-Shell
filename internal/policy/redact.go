package policy

import (
	"regexp"
	"strings"
)

// 输出侧脱敏。这是需求 4 的第三道防线：
// 即便命令本身合法（例如 cat 一个配置文件），它的输出里也可能夹带密钥。
// 所有回传给 LLM 的文本都必须先过这里。
//
// 重要前提：known 只包含**本机自己的**密钥（LLM API key + vault 里的 SSH 凭证）。
// 远端主机上的密钥不在其中 —— 所以下面这组模式的覆盖面，
// 就是远端密钥的实际保护边界。漏一类形态 = 那一类密钥会原样进到 LLM 的上下文。
//
// 最典型的漏网场景：`cat .env` / `env` / `docker inspect` / 各种 JSON 配置。
// 命令完全合法，输出里却是 DB_PASSWORD=hunter2 这种形态。

const redactedPlaceholder = "[REDACTED]"

// ---- 固定形状的凭据 ----
// 这些形态自带强特征，不需要判断上下文，直接整段抹掉。
var secretPatterns = []*regexp.Regexp{
	// PEM 私钥整块
	regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?s)-----BEGIN OPENSSH PRIVATE KEY-----.*?-----END OPENSSH PRIVATE KEY-----`),
	// 各类平台 token
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bASIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{20,}\b`),
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{20,}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9\-]{10,}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\b`), // JWT
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-]{16,}`),
	// 影子文件行（用户名:hash:…）。要求 $id$ 形态，才不会误伤 /etc/passwd 的 x 字段。
	regexp.MustCompile(`(?m)^[A-Za-z0-9_.\-]+:\$[0-9a-z]+\$[^\s:]+`),
}

// ---- 连接串 ----
// 只抹密码，保留 scheme / 用户名 / 主机 —— 否则 LLM 看不出这是连到哪，
// 而「能看懂连的是哪个库」恰恰是排查故障时最有用的信息。
var connStringRe = regexp.MustCompile(
	`(?i)\b(mysql|postgres|postgresql|mongodb|mongodb\+srv|redis|rediss|amqp|amqps)://([^\s:@/]+):([^\s@/]+)@`)

// ---- Authorization 头 ----
// 必须锚在 `Authorization:` 上。裸的 `\bbasic\s+\w+` 会把
// "basic authenticationrequired" 这类普通英文也当成凭据。
var authBasicRe = regexp.MustCompile(`(?i)(authorization\s*:\s*basic\s+)[A-Za-z0-9+/=]{16,}`)

// ---- 长选项形态 ----
// `--password hunter2`（空格分隔）。这类选项名足够明确，误伤风险极低。
// 注意 `--password=hunter2` 由下面的赋值形态覆盖，这里只管空格形态。
var longFlagRe = regexp.MustCompile(
	`(?i)(--(?:password|passwd|passphrase|token|api-key|apikey|secret|client-secret|private-key)\s+)(\S{4,})`)

// ---- 赋值形态 KEY = value ----
//
// 为什么不把「哪些键名算敏感」写进正则：
//   - RE2 不支持反向引用与环视，而键名有四种形态要同时容忍 ——
//     带前缀（DB_PASSWORD）、带后缀（SECRET_KEY_BASE）、被引号包住（"password"）、
//     无分隔粘连（PGPASSWORD）。用锚点硬凑必然顾此失彼；
//   - 原来用 `\b` 收边，而 `_` 与字母之间**不存在词边界**
//     （RE2 的 \w 含下划线），于是 DB_PASSWORD / MYSQL_ROOT_PASSWORD /
//     MY_TOKEN / JWT_SECRET / PGPASSWORD 整类漏掉 —— 恰恰是最典型的 .env 形态；
//   - 加一个敏感词就要重推一遍正则锚点，越改越容易漏。
//
// 所以正则只负责「把赋值形态抓出来」，敏感判定交给 isSensitiveKeyName。
// 它故意抓得很宽（任何 identifier : value 都会被抓），漏判靠键名分类器兜住。
var assignmentRe = regexp.MustCompile(
	// 前导：行首、空白、JSON/YAML 的结构符，以及引号 ——
	// 引号是必须的：`Environment="PASSWORD=hunter2"`（systemctl show / unit 文件）
	// 里键名紧跟在引号后面，少了这一项整类漏掉。
	`(?im)(?:^[ \t]*|[\s,;{(<|&\[\]"'])` +
		`(?:"([A-Za-z0-9_.\-]{2,64})"|'([A-Za-z0-9_.\-]{2,64})'|([A-Za-z0-9_.\-]{2,64}))` +
		`[ \t]*[:=][ \t]*` +
		// 值有三种形态：单引号串、双引号串、裸 token。
		// 引号形态必须整段吃掉 —— 否则 secret="correct horse" 只抹掉第一个词，
		// 输出看起来像已脱敏，实际泄漏后面全部（这比完全不脱敏更危险）。
		`(?:'[^'\n]{4,}'|"[^"\n]{4,}"|[^\s'"]{4,})`)

// exactLastWords：整个名字（忽略分隔符）等于它，或它的**最后一个组件**等于它，
// 即判定为敏感。
var exactLastWords = map[string]bool{
	"password": true, "passwd": true, "passphrase": true,
	"secret": true, "token": true,
	"apikey": true, "accesskey": true, "privatekey": true,
	"clientsecret": true, "secretkey": true,
	"authtoken": true, "accesstoken": true,
	"credential": true, "credentials": true,
}

// weakLastWords：单独出现太容易误伤，只在「至少两个组件、且它是最后一个」时才判定。
//   - pwd：`PWD` / `OLDPWD` 是每个 env 输出里都有的标准 shell 变量，
//     抹掉它们既没用又让用户以为软件坏了；而 `MYSQL_PWD` 仍能命中。
//   - pass：`bypass` / `compass` 是单个组件，不会命中；`DB_PASS` / `REDIS_PASS` 能命中。
var weakLastWords = map[string]bool{
	"pass": true, "pwd": true,
}

// affixWords：名字以它开头或结尾即判定，用于无分隔的粘连写法
// （PGPASSWORD / AWS_SECRET_ACCESS_KEY / SECRET_KEY_BASE）。
//
// **只收长度 >= 6 的词。** 短词做子串匹配会误伤正常标识符：
// token 会命中 max_tokens，key 会命中 monkey，secret 会命中 secretary。
var affixWords = []string{
	"password", "passphrase", "apikey", "apikeyid", "accesskey", "accesskeyid",
	"privatekey", "clientsecret", "secretkey", "authtoken", "accesstoken",
}

// normalizeKeyName 去掉分隔符并转小写：DB_PASSWORD → dbpassword、api_key → apikey。
func normalizeKeyName(name string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(name) {
		switch r {
		case '_', '-', '.', ' ', '"', '\'', '[', ']':
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// keyNameComponents 按 _ - . 拆成组件：MYSQL_ROOT_PASSWORD → [mysql root password]。
func keyNameComponents(name string) []string {
	return strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return r == '_' || r == '-' || r == '.'
	})
}

// isSensitiveKeyName 判断一个「键名」是否指向凭据。
//
// 三档判定，从最确定到最宽松。之所以能做得比正则准，
// 是因为这里的「包含」是**按组件**而不是按子串 —— `max_tokens` 不会被
// `token` 命中，因为它的组件是 max / tokens，而 tokens ≠ token。
func isSensitiveKeyName(name string) bool {
	name = strings.Trim(name, `"'`)
	if name == "" {
		return false
	}
	norm := normalizeKeyName(name)
	if norm == "" {
		return false
	}

	// 1) 整个名字就是敏感名（忽略分隔符）：password / api_key / apiKey / APIKEY
	if exactLastWords[norm] {
		return true
	}

	parts := keyNameComponents(name)
	if len(parts) > 0 {
		last := parts[len(parts)-1]
		// 2) 最后一个组件是敏感名：DB_PASSWORD / MY_TOKEN / CLIENT_SECRET
		//    只看最后一个组件 —— token_count、jwt_token_ttl、max_tokens
		//    这类名字里带敏感词但指的是别的量（个数、有效期），不该被抹掉。
		if exactLastWords[last] {
			return true
		}
		// 3) 弱词要求至少两个组件：DB_PASS / MYSQL_PWD 命中，OLDPWD / bypass 不命中
		if len(parts) >= 2 && weakLastWords[last] {
			return true
		}
	}

	// 4) 粘连写法，以长敏感词开头或结尾：
	//    PGPASSWORD / AWS_SECRET_ACCESS_KEY / SECRET_KEY_BASE
	for _, w := range affixWords {
		if strings.HasPrefix(norm, w) || strings.HasSuffix(norm, w) {
			return true
		}
	}
	return false
}

// Redact 对文本做脱敏，返回脱敏后的文本与替换次数。
//
// known 是「本机已知的密钥字面值」（来自 vault），它们优先级最高 ——
// 只要在输出里出现就整段替换，这是最强的保证：即使密钥以任意形态出现也不会外泄。
// 它同时是唯一能兜住「正则想不到的形态」的机制。
func Redact(text string, known []string) (string, int) {
	if text == "" {
		return text, 0
	}
	out := text
	n := 0

	// 1) 已知密钥字面值优先替换
	for _, s := range known {
		if len(s) < 4 { // 太短会在正常文本里到处误伤（与 vault.SecretStrings 的阈值一致）
			continue
		}
		if c := strings.Count(out, s); c > 0 {
			out = strings.ReplaceAll(out, s, redactedPlaceholder)
			n += c
		}
	}

	// 2) 连接串：先于赋值形态处理，这样 `DATABASE_URL=postgres://u:p@h/db`
	//    能保留主机名，而不是被整段值抹成 [REDACTED]
	out = connStringRe.ReplaceAllStringFunc(out, func(m string) string {
		n++
		sub := connStringRe.FindStringSubmatch(m)
		return sub[1] + "://" + sub[2] + ":" + redactedPlaceholder + "@"
	})

	// 3) Authorization: Basic <base64>
	out = authBasicRe.ReplaceAllStringFunc(out, func(m string) string {
		n++
		return authBasicRe.FindStringSubmatch(m)[1] + redactedPlaceholder
	})

	// 4) --password hunter2（空格分隔的长选项）
	out = longFlagRe.ReplaceAllStringFunc(out, func(m string) string {
		n++
		return longFlagRe.FindStringSubmatch(m)[1] + redactedPlaceholder
	})

	// 5) 已知命令的 -p 短选项（mysql -psecret / sshpass -p hunter2）。
	//    必须放在命令上下文里才认，理由见 shortPassCommands。
	out, n = redactShortPassOptions(out, n)

	// 6) KEY = value。用 FindAllStringSubmatchIndex 一次扫完再重建，
	//    而不是「匹配一次、替换一次、再扫」—— 后者会让 [REDACTED] 被后续轮次
	//    再次命中（`password = [REDACTED]` 本身就符合赋值形态），把计数越滚越高。
	out, n = redactAssignments(out, n)

	// 7) 固定形状
	for _, re := range secretPatterns {
		out = re.ReplaceAllStringFunc(out, func(m string) string {
			n++
			// 保留 key= / user: 的前缀，只抹掉值，便于 LLM 理解上下文
			if i := strings.IndexAny(m, "=:"); i > 0 && i < 40 && !strings.Contains(m, "\n") {
				return m[:i+1] + " " + redactedPlaceholder
			}
			return redactedPlaceholder
		})
	}
	return out, n
}

// shortPassRule 描述某条命令的 `-p` 短选项怎么取值。
type shortPassRule struct {
	attached bool // -pVALUE（粘连）
	spaced   bool // -p VALUE（空格分隔）
}

// shortPassCommands：`-p` 在这些命令里**明确**表示密码。
//
// 为什么必须带命令上下文：`-p` 单独看歧义极大 ——
// mysql 家族里是密码，ssh 里是端口，mkdir 里是权限模式，docker run 里是端口映射。
// 无脑按前缀猜会大面积误伤，而误伤会让模型分析不了故障。
// 所以只在能确定命令身份时才认这一条；其余场合宁可漏（见 KnownGaps 用例）。
//
// attached / spaced 的区分来自命令自身的语法：
//   - mysql 家族：`-p` 必须粘连（`-psecret`）。写成 `-p secret` 时
//     mysql 会把 secret 当成**库名**，那时它根本不是密码 —— 认了就是误伤。
//   - sshpass：两种写法都接受。
var shortPassCommands = map[string]shortPassRule{
	"mysql":       {attached: true},
	"mariadb":     {attached: true},
	"mysqladmin":  {attached: true},
	"mysqldump":   {attached: true},
	"mysqlimport": {attached: true},
	"mysqlshow":   {attached: true},
	"sshpass":     {attached: true, spaced: true},
}

// commandWrappers 是「真正的命令名在它后面」的包装词，
// 扫命令名时要跳过它们（`sudo mysql -px` 里的命令是 mysql）。
var commandWrappers = map[string]bool{
	"sudo": true, "doas": true, "env": true, "nohup": true, "time": true,
	"command": true, "exec": true, "nice": true, "ionice": true,
}

// segmentRe 按 shell 命令分隔符切段：`mysql -psecret | tee x` 是两段。
// 与 policy.go 的 splitSegments 用同一组分隔符。
var segmentRe = regexp.MustCompile(`[^|;&\n]+`)

// wordRe 取段里的词，用于找命令名。
var wordRe = regexp.MustCompile(`[^\s"'=]+`)

// redactShortPassOptions 抹掉「已知命令的 -p 短选项」里的密码。
//
// 这是本文件里第二条**结构感知**规则（另一条是 yamlEnvPair）：
// 只有先确定命令身份，`-p` 的含义才唯一。做法是按命令分隔符切段，
// 在段内找已知命令名，再抹它后面 `-p` 的值。
func redactShortPassOptions(text string, n int) (string, int) {
	if !strings.Contains(text, "-p") {
		return text, n // 绝大多数输出不含 -p，早退省掉整轮扫描
	}
	var sb strings.Builder
	last := 0
	hits := 0
	for _, loc := range segmentRe.FindAllStringIndex(text, -1) {
		out, c := redactShortPassInSegment(text[loc[0]:loc[1]])
		if c == 0 {
			continue
		}
		sb.WriteString(text[last:loc[0]])
		sb.WriteString(out)
		last = loc[1]
		hits += c
	}
	if hits == 0 {
		return text, n
	}
	sb.WriteString(text[last:])
	return sb.String(), n + hits
}

// redactShortPassInSegment 在单个命令段内处理 `-p`。
func redactShortPassInSegment(seg string) (string, int) {
	rule, cmdEnd, ok := findShortPassRule(seg)
	if !ok {
		return seg, 0
	}
	return replaceShortPass(seg, rule, cmdEnd)
}

// findShortPassRule 扫出段里的命令名，返回 `-p` 规则与命令词的结束位置。
//
// 扫**所有**词而不是只看段首：`ps` 输出里命令名不在开头
// （`root 1234 mysql -psecret`），只看第一个词会整类漏掉。
//
// 同时返回命令词的结束位置，因为调用方**只能处理它之后的 `-p`** ——
// 实测踩过：`docker run -p 3306:3306 sshpass` 里的 `-p` 属于 docker
// （端口映射），若不加位置限制就会被当成 sshpass 的密码，
// 把端口号 `3306:3306` 抹掉。
func findShortPassRule(seg string) (shortPassRule, int, bool) {
	for _, loc := range wordRe.FindAllStringIndex(seg, -1) {
		low := strings.ToLower(seg[loc[0]:loc[1]])
		if commandWrappers[low] {
			continue
		}
		if i := strings.LastIndexByte(low, '/'); i >= 0 {
			low = low[i+1:] // /usr/bin/mysql → mysql
		}
		if r, ok := shortPassCommands[low]; ok {
			return r, loc[1], true
		}
	}
	return shortPassRule{}, 0, false
}

// replaceShortPass 抹掉段内 `-p` 短选项的值，只处理 minPos 之后的部分。
//
// 手写扫描而不是两条正则顺序套用：先跑粘连、再跑空格分隔的话，
// 第一条把 `-psecret` 改成 `-p [REDACTED]` 之后，第二条会再命中一次，
// 计数被重复累加（占位符本身看着就像个值）。
func replaceShortPass(seg string, rule shortPassRule, minPos int) (string, int) {
	var sb strings.Builder
	last := 0
	n := 0
	i := minPos
	for i < len(seg) {
		j := strings.Index(seg[i:], "-p")
		if j < 0 {
			break
		}
		p := i + j
		i = p + 2
		// 必须是独立的短选项：前面是段首或空白。
		// 这一条同时挡住 `--password`（第二个 `-` 前面不是空白）
		// 与 `[-f|-d|-p|-e]` 这种用法说明（`-p` 前面是 `|`）。
		if p > 0 && !isSpaceByte(seg[p-1]) {
			continue
		}

		valStart, valEnd := -1, -1
		if rule.attached && i < len(seg) && !isSpaceByte(seg[i]) && !isQuoteByte(seg[i]) {
			valStart, valEnd = i, tokenEnd(seg, i)
		} else if rule.spaced {
			m := i
			for m < len(seg) && isSpaceByte(seg[m]) {
				m++
			}
			// 必须真的有空白分隔，否则那是粘连形态（上面已处理过）
			if m > i && m < len(seg) && !isQuoteByte(seg[m]) {
				valStart, valEnd = m, tokenEnd(seg, m)
			}
		}
		if valStart < 0 || !looksLikeSecretValue(seg[valStart:valEnd]) {
			continue
		}

		sb.WriteString(seg[last:valStart])
		sb.WriteString(redactedPlaceholder)
		last = valEnd
		i = valEnd
		n++
	}
	if n == 0 {
		return seg, 0
	}
	sb.WriteString(seg[last:])
	return sb.String(), n
}

// looksLikeSecretValue 排除明显不是密钥的取值，避免误伤。
//
// 主要是挡住用法/帮助文本：`mysql -p, --password` 与 `[-f|-d|-p|-e]`
// 这类写法里的「值」其实是语法符号，抹掉会把文本改花，让模型看不懂文档。
// 占位符（`<password>` / `[password]` / `{password}`）同理跳过 ——
// 它们本来就不是密钥。
//
// 判据是「首字符是不是 shell/语法标点」。口令极少以这些字符开头，
// 而它们恰恰是语法文本的典型开头。
func looksLikeSecretValue(v string) bool {
	if v == "" {
		return false
	}
	switch v[0] {
	case ',', '-', '<', '[', '{', '=', '+', '|', ']', ')', '}', ';', '&', '>', '*', '?':
		return false
	}
	return true
}

func isSpaceByte(b byte) bool { return b == ' ' || b == '\t' }
func isQuoteByte(b byte) bool { return b == '"' || b == '\'' }

// tokenEnd 返回从 i 开始的一个「词」的结束位置。
func tokenEnd(s string, i int) int {
	for i < len(s) && !isSpaceByte(s[i]) && !isQuoteByte(s[i]) {
		i++
	}
	return i
}

// redactAssignments 抹掉「敏感键名 = 值」里的值，保留键名与分隔符。
//
// 手写扫描循环而不是 FindAllStringSubmatchIndex，是因为必须处理**嵌套**：
//
//	Environment=PASSWORD=hunter2
//	Environment="PASSWORD=hunter2"
//
// 这两种形态里，外层键名（Environment）不敏感，真正的凭据在内层。
// 用 FindAll 的话，最左匹配会把整段吃掉，扫描从匹配末尾继续，
// 内层就再也没机会被看到 —— 而 `systemctl show` / unit 文件里
// 恰恰就是这种 `Environment=VAR=value` 的写法。
//
// 所以：键名不敏感时**从分隔符之后**继续找，而不是跳到整个匹配的末尾。
func redactAssignments(text string, n int) (string, int) {
	var sb strings.Builder
	sb.Grow(len(text))
	last := 0 // 已经写出的文本末尾
	pos := 0  // 搜索起点
	hit := false

	// envNameEnd 记住「上一处是 `name: <敏感键名>`」的位置，
	// 于是紧随其后的 `value:` 里装的才是真密钥。见 yamlEnvPair 的说明。
	envNameEnd := -1

	for pos < len(text) {
		m := assignmentRe.FindStringSubmatchIndex(text[pos:])
		if m == nil {
			break
		}
		// 子匹配下标是相对 text[pos:] 的，转成绝对下标
		keyStart, keyEnd := -1, -1
		switch {
		case m[2] >= 0:
			keyStart, keyEnd = pos+m[2], pos+m[3]
		case m[4] >= 0:
			keyStart, keyEnd = pos+m[4], pos+m[5]
		case m[6] >= 0:
			keyStart, keyEnd = pos+m[6], pos+m[7]
		}
		matchEnd := pos + m[1]
		if keyStart < 0 {
			pos = matchEnd
			continue
		}

		// 分隔符在键名之后。键名字符集不含 = :，但从键尾开始找最省事也最稳。
		sep := -1
		for i := keyEnd; i < matchEnd; i++ {
			if text[i] == '=' || text[i] == ':' {
				sep = i
				break
			}
		}
		if sep < 0 {
			pos = matchEnd
			continue
		}

		if !yamlEnvPair(text[keyStart:keyEnd], valueSpan(text, sep, matchEnd), keyStart, &envNameEnd) &&
			!isSensitiveKeyName(text[keyStart:keyEnd]) {
			pos = sep + 1 // 见函数注释：往值里面继续找，别跳过嵌套的凭据
			continue
		}

		sb.WriteString(text[last : sep+1]) // 前缀 + 键名 + 分隔符
		sb.WriteString(" ")
		sb.WriteString(redactedPlaceholder)
		last = matchEnd
		pos = matchEnd
		n++
		hit = true
	}
	if !hit {
		return text, n // 一个都没命中，别做无谓的重建
	}
	sb.WriteString(text[last:])
	return sb.String(), n
}

// yamlEnvPairWindow 限制 name: 与 value: 之间允许的距离（字节）。
//
// 同一列表项内两者必然紧邻（换行 + 缩进，几个字节）。
// 设上限是为了挡住「敏感的 name 之后隔了很远才出现一个无关的 value:」——
// 那种情况下把后者抹掉属于误伤，而误伤同样是 bug（模型会因此分析不了故障）。
const yamlEnvPairWindow = 256

// yamlEnvPair 处理「键名与密钥分处两行」的 env 块形态，返回当前这处是否要脱敏。
//
//	kubectl get pod -o yaml:   - name: DB_PASSWORD
//	                             value: hunter2
//	docker inspect / compose:   同样形状
//
// 为什么单靠赋值规则抓不到：`name` 与 `value` 这两个键名本身都不敏感，
// 敏感的是 **name 的值**（一个键名），而真密钥在**下一行** value 的值里。
// 任何「只看一行」的规则都必然漏掉它。
//
// pendingEnd 记录上一个敏感 name 匹配的结束位置（-1 表示没有），
// 是指针，因为状态要跨匹配延续。
func yamlEnvPair(key, val string, keyStart int, pendingEnd *int) bool {
	switch strings.ToLower(strings.Trim(key, `"'`)) {
	case "name":
		// 这一行本身不是密钥 —— 值只是个**键名**，抹掉它反而丢了排查线索。
		// 但它决定了紧随其后的 value: 是不是密钥。
		if isSensitiveKeyName(val) {
			*pendingEnd = keyStart // 用 keyStart 作锚点，下面按同一坐标系比距离
		} else {
			*pendingEnd = -1
		}
		return false
	case "value":
		matched := *pendingEnd >= 0 && keyStart-*pendingEnd <= yamlEnvPairWindow
		*pendingEnd = -1 // 无论是否命中，配对到此结束
		return matched
	}
	// 不是 name/value —— 配对断了，别让它跨过别的键继续生效。
	*pendingEnd = -1
	return false
}

// valueSpan 取出一处赋值里「值」的文本（跳过分隔符后的空白）。
// 用于把 name 的值交给 isSensitiveKeyName 判断。
func valueSpan(text string, sep, matchEnd int) string {
	i := sep + 1
	for i < matchEnd && (text[i] == ' ' || text[i] == '\t') {
		i++
	}
	return text[i:matchEnd]
}

// 说明：本文件此前还导出过 HasSecret 与 RedactNotice，二者都已删除。
//
//   - HasSecret 在生产路径里从未被调用。生产用的是 vault.Host.HasSecret 字段 ——
//     那是配置期「这台主机是否已配置密钥材料」的判断，与文本扫描无关。
//     保留一个同名但语义完全不同的导出函数，只会误导后续维护者。
//   - RedactNotice 与 injection.go 里的 redactNoticeBody 是同一段文案的两份拷贝，
//     而 PrepareForLLM 用的是后者。改前者不会有任何行为变化 —— 典型的
//     「改了没反应」陷阱。
//
// 另需注意一个反直觉的点：**脱敏后的文本仍然匹配这些模式**。
// 例如 `password = [REDACTED]` 依然符合赋值形态，因为占位符本身就是
// 「4 个以上非空白字符」。所以不要用「再扫一遍模式」去验证脱敏是否彻底 ——
// 那样只会永远报警。要验证彻底性，应当断言输出里不再包含**原始密钥字面值**
// （见 redact_test.go 的写法）。
