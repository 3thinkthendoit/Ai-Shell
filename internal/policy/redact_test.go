package policy

import (
	"strings"
	"testing"
)

// 脱敏是需求 4 的第三道防线，而且它的失效方式很隐蔽：
// 命令本身完全合法（`cat .env`、`env`、`git config --list`），
// 但输出里夹着远端主机的密钥，会原样进到 LLM 的上下文里。
//
// 注意 known 只包含**本机自己的**密钥（LLM key + vault 里的 SSH 凭证）。
// 远端主机上的密钥不在其中，所以完全依赖下面的正则 —— 这组用例锁的就是它。

// ---- 必须脱敏 ----

func TestRedactsCredentialShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// 输出里绝不能再出现的东西
		mustNotContain string
	}{
		{"PEM 私钥整块", "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----", "MIIEowIBAAKCAQEA"},
		{"OpenSSH 私钥整块", "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk\n-----END OPENSSH PRIVATE KEY-----", "b3BlbnNzaC1rZXk"},
		{"AWS Access Key ID", "aws_access_key_id = AKIAIOSFODNN7EXAMPLE", "AKIAIOSFODNN7EXAMPLE"},
		{"AWS 临时凭证 ID", "ASIAIOSFODNN7EXAMPLE", "ASIAIOSFODNN7EXAMPLE"},
		{"OpenAI 风格 key", "OPENAI_KEY=sk-proj-abcdefghijklmnopqrstuvwxyz", "sk-proj-abcdefghijklmnopqrstuvwxyz"},
		{"Anthropic 风格 key", "sk-ant-api03-abcdefghijklmnopqrstuvwxyz", "sk-ant-api03-abcdefghijklmnopqrstuvwxyz"},
		{"GitHub token", "token: ghp_abcdefghijklmnopqrstuvwxyz0123456789", "ghp_abcdefghijklmnopqrstuvwxyz0123456789"},
		{"GitHub PAT", "github_pat_11ABCDEFG0abcdefghijklmnopqrstuvwxyz", "github_pat_11ABCDEFG0abcdefghijklmnopqrstuvwxyz"},
		{"Slack token", "xoxb-000000000000-TESTONLY_NOT_A_REAL_TOKEN", "xoxb-000000000000-TESTONLY_NOT_A_REAL_TOKEN"},
		{"JWT", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1g", "eyJzdWIiOiIxMjM0NTY3ODkwIn0"},
		{"Bearer token", "Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345", "abcdefghijklmnopqrstuvwxyz012345"},
		{"等号赋值 password", "password=hunter2", "hunter2"},
		{"冒号赋值 Password（大小写）", "Password : hunter2", "hunter2"},
		{"passwd 字段", "passwd=hunter2", "hunter2"},
		{"api_key 字段", "api_key=abcdef123456", "abcdef123456"},
		{"token 字段", "token: abcdef123456", "abcdef123456"},
		{"client_secret 字段", "client_secret = abcdef123456", "abcdef123456"},
		{"MySQL 连接串", "mysql://root:s3cr3tpass@10.0.0.1:3306/app", "s3cr3tpass"},
		{"Postgres 连接串", "postgresql://admin:p4ssw0rd@db.internal/app", "p4ssw0rd"},
		{"Redis 连接串", "redis://default:r3dispass@cache:6379", "r3dispass"},
		{"/etc/shadow 行", "root:$6$abcDEF123$xyzABCdef456:19000:0:99999:7:::", "$6$abcDEF123$xyzABCdef456"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, n := Redact(c.in, nil)
			if n == 0 {
				t.Fatalf("一次都没替换：%q", c.in)
			}
			if strings.Contains(out, c.mustNotContain) {
				t.Fatalf("密钥泄漏了\n  输入: %s\n  输出: %s", c.in, out)
			}
		})
	}
}

// ---- 已知密钥字面值优先级最高 ----

// known 里的值可能以任意形态出现（不带 key= 前缀、出现在奇怪的位置），
// 所以它是唯一能兜住「正则想不到的形态」的机制，必须真的生效。
func TestKnownSecretsAreReplacedVerbatim(t *testing.T) {
	known := []string{"Sup3rS3cretPassphrase", "AAAAB3NzaC1yc2EAAAADAQABAAABgQ"}

	out, n := Redact("connecting with Sup3rS3cretPassphrase now", known)
	if strings.Contains(out, "Sup3rS3cretPassphrase") {
		t.Fatalf("known 字面值没被替换: %s", out)
	}
	if n != 1 {
		t.Fatalf("应替换 1 次，实得 %d", n)
	}

	// 私钥正文的一行（不含任何 key= 前缀）也要能兜住
	out2, _ := Redact("blob AAAAB3NzaC1yc2EAAAADAQABAAABgQ end", known)
	if strings.Contains(out2, "AAAAB3NzaC1yc2EAAAADAQABAAABgQ") {
		t.Fatalf("known 私钥行没被替换: %s", out2)
	}
}

// 太短的值不参与字面值替换：4 个字符以内会在正常文本里到处误伤。
// （注意：Redact 里 len<4 跳过，vault.SecretStrings 里也是 len<4 不收集，两道一致。）
func TestShortKnownSecretsAreIgnored(t *testing.T) {
	out, n := Redact("the cat sat on the mat", []string{"cat", "the"})
	if n != 0 {
		t.Fatalf("过短的字面值不该参与替换，实得 %d 次", n)
	}
	if out != "the cat sat on the mat" {
		t.Fatalf("文本被改动了: %s", out)
	}
}

// ---- 键名分类器：单测它比单测整条管线更能定位问题 ----

// 这批用例来自实测：原来的实现用 `\b` 收边，而 `_` 与字母之间**不存在词边界**
// （RE2 的 \w 含下划线），于是带前缀的键名整类漏掉 —— 恰恰是 `.env` 最常见的形态。
func TestSensitiveKeyNameClassifier(t *testing.T) {
	sensitive := []string{
		"password", "Password", "PASSWORD", "passwd", "passphrase",
		"secret", "token", "api_key", "apiKey", "APIKEY", "apikey",
		"DB_PASSWORD", "MYSQL_ROOT_PASSWORD", "DATABASE_PASSWORD", "PGPASSWORD",
		"MY_TOKEN", "JWT_SECRET", "SECRET_KEY_BASE", "AWS_SECRET_ACCESS_KEY",
		"CLIENT_SECRET", "ACCESS_KEY", "PRIVATE_KEY", "AUTH_TOKEN",
		"DB_PASS", "REDIS_PASS", "MYSQL_PWD",
		"db.password", "my-secret",
	}
	for _, k := range sensitive {
		if !isSensitiveKeyName(k) {
			t.Errorf("应判定为敏感键名: %q", k)
		}
	}

	// 误伤比漏网更容易被忽略：把正常配置抹掉会让 LLM 分析不了故障，
	// 用户也会以为工具坏了。这些都必须判为「不敏感」。
	benign := []string{
		"path", "Path", "PWD", "OLDPWD", // PWD/OLDPWD 是每个 env 输出都有的标准变量
		"max_tokens", "TOKEN_COUNT", "JWT_TOKEN_TTL", "TOKENS",
		"bypass", "compass", "compass_mode", "passenger_count",
		"monkey", "key", "KEY", "keyfile", "publickey", "keyboard_layout",
		"secretary", "secretary_name",
		"kind", "name", "memory", "total", "tasks", "loaded", "active", "docs",
		"ssl_certificate_key", "username", "host", "port",
	}
	for _, k := range benign {
		if isSensitiveKeyName(k) {
			t.Errorf("不该判定为敏感键名（会误伤）: %q", k)
		}
	}
}

// ---- 实测确认过的漏网形态，逐条固化成回归 ----

// `cat .env` / `env` / `printenv` 的输出。命令完全合法，
// 输出里却全是凭据 —— 这是最可能真实发生的泄漏场景。
func TestRedactsEnvDumpShapes(t *testing.T) {
	leaks := map[string]string{
		"DB_PASSWORD=hunter2":                                    "hunter2",
		"MYSQL_ROOT_PASSWORD=hunter2":                            "hunter2",
		"DATABASE_PASSWORD=hunter2":                              "hunter2",
		"PGPASSWORD=hunter2":                                     "hunter2",
		"export DB_PASS=hunter2":                                 "hunter2",
		"export REDIS_PASS=r3dispass":                            "r3dispass",
		"AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEX": "wJalrXUtnFEMI",
		"SECRET_KEY_BASE=abcdef1234567890abcdef":                 "abcdef1234567890abcdef",
		"JWT_SECRET=abcdef1234567890":                            "abcdef1234567890",
		"MY_TOKEN=abcdef123456":                                  "abcdef123456",
		"AUTH_TOKEN=abcdef123456":                                "abcdef123456",
		"CLIENT_SECRET=abcdef123456":                             "abcdef123456",
		"Environment=\"PASSWORD=hunter2\"":                       "hunter2",
		"Environment='DB_PASSWORD=hunter2'":                      "hunter2",
	}
	for in, leak := range leaks {
		out, n := Redact(in, nil)
		if n == 0 {
			t.Errorf("完全没脱敏（整类漏网）\n  输入: %s", in)
			continue
		}
		if strings.Contains(out, leak) {
			t.Errorf("密钥泄漏\n  输入: %s\n  输出: %s", in, out)
		}
	}
}

// JSON / YAML 配置的输出（`cat config.json`、`docker inspect`、`kubectl get -o yaml`）。
// 键名被引号包住是这里的特征 —— 原实现里 `password` 后面紧跟 `"` 而不是 `=`，整条不匹配。
func TestRedactsJSONAndYAMLShapes(t *testing.T) {
	cases := []struct{ in, leak string }{
		{`{"password":"jsonvalue123"}`, "jsonvalue123"},
		{`{"api_key": "jsonvalue123"}`, "jsonvalue123"},
		{`{"db_password": "jsonvalue123"}`, "jsonvalue123"},
		{`{'secret': 'jsonvalue123'}`, "jsonvalue123"},
		{`"Password": "jsonvalue123",`, "jsonvalue123"},
		{`{"auth":{"token":"jsonvalue123"}}`, "jsonvalue123"},
		{`  "PASSWORD": "jsonvalue123"`, "jsonvalue123"},
	}
	for _, c := range cases {
		out, n := Redact(c.in, nil)
		if n == 0 {
			t.Errorf("完全没脱敏\n  输入: %s", c.in)
			continue
		}
		if strings.Contains(out, c.leak) {
			t.Errorf("密钥泄漏\n  输入: %s\n  输出: %s", c.in, out)
		}
	}
}

// 值被引号包住、且里面有空格。
// 原实现只吃掉第一个词，输出是 `secret= [REDACTED] horse battery staple"` ——
// **看起来像已脱敏，实际泄漏后面全部**，这比完全不脱敏更危险。
func TestRedactsQuotedValuesWithSpaces(t *testing.T) {
	cases := []struct{ in, leak string }{
		{`password = "my secret pass"`, "secret pass"},
		{`secret="correct horse battery staple"`, "battery staple"},
		{`passphrase = 'open the pod bay doors'`, "pod bay doors"},
	}
	for _, c := range cases {
		out, _ := Redact(c.in, nil)
		if strings.Contains(out, c.leak) {
			t.Errorf("引号串只被吃了一部分\n  输入: %s\n  输出: %s", c.in, out)
		}
	}
}

// `Authorization: Basic <base64>` 里是 base64 的 user:password。
// 必须锚在 Authorization 上：裸的 `\bbasic\s+\w+` 会把普通英文也当成凭据。
func TestRedactsAuthorizationBasic(t *testing.T) {
	out, n := Redact("Authorization: Basic dXNlcjpwYXNzd29yZA==", nil)
	if n == 0 {
		t.Fatal("Authorization: Basic 没被脱敏")
	}
	if strings.Contains(out, "dXNlcjpwYXNzd29yZA==") {
		t.Fatalf("Basic 凭据泄漏: %s", out)
	}
}

// `--password hunter2`（空格分隔）。`--password=hunter2` 由赋值形态覆盖。
func TestRedactsLongFlagsWithSpace(t *testing.T) {
	cases := []struct{ in, leak string }{
		{"mysql --password hunter2 -u root", "hunter2"},
		{"app --api-key abcdef123456", "abcdef123456"},
		{"app --client-secret abcdef123456", "abcdef123456"},
	}
	for _, c := range cases {
		out, _ := Redact(c.in, nil)
		if strings.Contains(out, c.leak) {
			t.Errorf("长选项的值泄漏\n  输入: %s\n  输出: %s", c.in, out)
		}
	}
}

// 连接串要保留 scheme / 用户名 / 主机：LLM 需要知道「连的是哪个库」才能排查故障。
// 原实现把整段 `postgres://u:p@` 抹成 `postgres: [REDACTED]`，主机名也一起没了。
func TestConnStringKeepsHost(t *testing.T) {
	out, n := Redact("DATABASE_URL=postgres://app:p4ssw0rd@db.internal:5432/prod", nil)
	if n == 0 {
		t.Fatal("连接串没被脱敏")
	}
	if strings.Contains(out, "p4ssw0rd") {
		t.Fatalf("密码泄漏: %s", out)
	}
	if !strings.Contains(out, "db.internal") {
		t.Fatalf("主机名不该被抹掉（否则 LLM 看不出连的是哪个库）: %s", out)
	}
}

// ---- 嵌套赋值：外层键名不敏感，真正的凭据在内层 ----
//
// `Environment=VAR=value` 是 `systemctl show` / unit 文件 / 容器 spec 里的常见写法。
// 这里踩过一个真 bug：用最左匹配（FindAll）时，外层匹配会把整段吃掉，
// 扫描从匹配末尾继续，内层再也没机会被看到 —— 整类漏掉。
// 修法是键名不敏感时**从分隔符之后**继续找，而不是跳到匹配末尾。
func TestRedactsNestedAssignments(t *testing.T) {
	cases := []struct{ in, leak string }{
		{"Environment=PASSWORD=hunter2", "hunter2"},
		{`Environment="PASSWORD=hunter2"`, "hunter2"},
		{`Environment='DB_PASSWORD=hunter2'`, "hunter2"},
		{"Environment=DB_PASSWORD=hunter2", "hunter2"},
		{"Environment=MY_TOKEN=abcdef123456", "abcdef123456"},
		{`ExecStart=/usr/bin/app --token abcdef123456`, "abcdef123456"},
	}
	for _, c := range cases {
		out, n := Redact(c.in, nil)
		if n == 0 {
			t.Errorf("嵌套的凭据整段漏掉了\n  输入: %s", c.in)
			continue
		}
		if strings.Contains(out, c.leak) {
			t.Errorf("密钥泄漏\n  输入: %s\n  输出: %s", c.in, out)
		}
	}
}

// ---- 已知命令的 -p 短选项 ----
//
// `-p` 单独看歧义极大：mysql 家族里是密码，ssh 里是端口，mkdir 里是权限模式，
// docker run 里是端口映射。所以只在**能确定命令身份**时才认这一条 ——
// 做法是按命令分隔符切段、段内找已知命令名，再抹它后面 `-p` 的值。
//
// 漏掉的场合（如 `grep -p`、`smbclient -p`）有意保留：不认识的命令不猜。
func TestRedactsShortPasswordOptions(t *testing.T) {
	cases := []struct {
		name string
		in   string
		leak string
	}{
		{"mysql 粘连", "mysql -phunter2", "hunter2"},
		{"mariadb 粘连", "mariadb -pS3cr3t-db-pw", "S3cr3t-db-pw"},
		{"mysqldump 粘连", "mysqldump -pS3cr3t-db-pw mydb > dump.sql", "S3cr3t-db-pw"},
		{"带其他选项", "mysql -h db.internal -u root -pS3cr3t-db-pw mydb", "S3cr3t-db-pw"},
		{"绝对路径", "/usr/bin/mysql -pS3cr3t-db-pw mydb", "S3cr3t-db-pw"},
		{"sudo 包装", "sudo mysql -pS3cr3t-db-pw mydb", "S3cr3t-db-pw"},
		{"sshpass 空格分隔", "sshpass -p hunter2 ssh root@host", "hunter2"},
		{"sshpass 粘连", "sshpass -phunter2 ssh root@host", "hunter2"},
		// ps 输出里命令名不在段首 —— 只看第一个词会整类漏掉。
		{"ps 输出形态", "root 1234 0.0 0.1 123456 7890 ? Ssl 10:00 0:01 mysql -pS3cr3t-db-pw", "S3cr3t-db-pw"},
		// 分段：管道/分号之后的段要各自处理。
		{"管道后一段", "cat x | mysql -pS3cr3t-db-pw mydb", "S3cr3t-db-pw"},
		{"分号后一段", "echo start; mysql -pS3cr3t-db-pw mydb", "S3cr3t-db-pw"},
	}
	for _, c := range cases {
		out, n := Redact(c.in, nil)
		if n == 0 {
			t.Errorf("%s：没有脱敏\n  输入: %s", c.name, c.in)
			continue
		}
		if strings.Contains(out, c.leak) {
			t.Errorf("%s：密码泄漏\n  输入: %s\n  输出: %s", c.name, c.in, out)
		}
	}
}

// 短选项的反向断言：这些 `-p` 都不是密码，一个都不许动。
//
// 这一组比「必须脱敏」更重要：`-p` 是最常见的短选项之一，
// 认错一次就会把正常参数（端口、权限模式、路径）抹掉，
// 让模型分析不了故障，用户也会以为工具坏了。
func TestShortPasswordOptionsDoNotOverRedact(t *testing.T) {
	keep := []struct{ name, in, mustKeep string }{
		{"大写 P 是端口（mysql）", "mysql -P3306 -h db.internal", "3306"},
		{"mysql 的空格形态是库名不是密码", "mysql -p secretdb", "secretdb"},
		{"ssh 的 -p 是端口", "ssh -p 2222 root@host", "2222"},
		{"scp 的 -p 是端口", "scp -p 2222 file root@host:/tmp", "2222"},
		{"mkdir 的 -p 是建父目录", "mkdir -p /var/log/app", "/var/log/app"},
		{"tar 的 -p 是保留权限", "tar -p -cf etc.tar /etc", "etc.tar"},
		{"rsync 的 -p 是保留权限", "rsync -p src/ dst/", "src/"},
		// 关键一条：`-p` 在命令名**之前**，属于 docker 而非 sshpass。
		// 加位置约束之前，这里会把端口号当成密码抹掉（实测踩过）。
		{"docker 端口映射在命令名之前", "docker run -p 3306:3306 mysql", "3306:3306"},
		{"同上，镜像换成 sshpass", "docker run -p 3306:3306 sshpass", "3306:3306"},
		{"用法说明里的 -p", "usage: sshpass [-f|-d|-p|-e] command", "[-f|-d|-p|-e]"},
		{"帮助文本 -p, --password", "mysql -p, --password", "--password"},
		{"占位符不是密钥", "sshpass -p <password> ssh host", "<password>"},
		{"没有值的 -p", "mysql -p -u root", "-u root"},
		{"不认识的命令不猜", "grep -p pattern file", "pattern"},
	}
	for _, c := range keep {
		out, _ := Redact(c.in, nil)
		if !strings.Contains(out, c.mustKeep) {
			t.Errorf("%s：被误伤（%q 不该被抹）\n  输入: %s\n  输出: %s",
				c.name, c.mustKeep, c.in, out)
		}
	}
}

// ---- 已知缺口 ----
//
// 这些形态**目前不脱敏**。刻意写成断言而不是注释：将来补上时这条用例会失败，
// 提醒作者把它翻成「必须脱敏」。不要把它们当成「正确行为」——
// 每一条都是真实存在的泄漏路径，只是收益/风险比暂时不划算。
//
// 2026-09-28：原有的三条缺口已全部补上，本清单**当前为空**。
//   - `- name: DB_PASSWORD` + `value: hunter2` → 见 TestRedactsYAMLEnvBlocks
//   - `mysql -phunter2` / `sshpass -p hunter2` → 见 TestRedactsShortPasswordOptions
//
// 保留这个函数是因为**机制本身有用**：发现新的「知道会漏但暂时不做」的形态时，
// 写进 gaps 而不是写进注释 —— 补上那天它会自己变红提醒你翻断言。
func TestKnownGapsNotYetRedacted(t *testing.T) {
	gaps := []struct{ in, why string }{
		// 当前没有已知缺口。新增时按上面的格式补一条。
	}
	for _, g := range gaps {
		out, n := Redact(g.in, nil)
		if n > 0 {
			t.Errorf("这个缺口已经被补上了，请把本用例翻成「必须脱敏」：\n  输入: %s\n  输出: %s\n  原因: %s",
				g.in, out, g.why)
		}
	}
}

// ---- YAML / JSON 的 env 块：键名与密钥分处两行 ----
//
// `kubectl get pod -o yaml`、`docker inspect`、docker-compose 的 env 块都是这个形状：
//
//   - name: DB_PASSWORD
//     value: hunter2
//
// 难点在于**单看任意一行都不敏感**：`name` 与 `value` 都是普通键名，
// 敏感的是 name 的**值**（一个键名），而真密钥在下一行 value 的值里。
// 所以任何「只看一行」的规则都抓不到，必须做跨行的结构感知。
func TestRedactsYAMLEnvBlocks(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		leak    string
		keyName string // 必须保留的键名（模型要靠它知道哪个变量有问题）
	}{
		{
			name:    "kubectl env 列表（缩进）",
			in:      "env:\n- name: DB_PASSWORD\n  value: hunter2\n- name: LOG_LEVEL\n  value: debug",
			leak:    "hunter2",
			keyName: "DB_PASSWORD",
		},
		{
			name:    "docker-compose 缩进更深",
			in:      "    environment:\n      - name: MYSQL_ROOT_PASSWORD\n        value: s3cr3t-pw",
			leak:    "s3cr3t-pw",
			keyName: "MYSQL_ROOT_PASSWORD",
		},
		{
			name:    "JSON 单行形态",
			in:      `{"name": "API_TOKEN", "value": "abcdef123456"}`,
			leak:    "abcdef123456",
			keyName: "API_TOKEN",
		},
		{
			name:    "值带引号",
			in:      "- name: CLIENT_SECRET\n  value: \"abcdef123456\"",
			leak:    "abcdef123456",
			keyName: "CLIENT_SECRET",
		},
	}
	for _, c := range cases {
		out, n := Redact(c.in, nil)
		if n == 0 {
			t.Errorf("%s：没有脱敏\n  输入: %s", c.name, c.in)
			continue
		}
		if strings.Contains(out, c.leak) {
			t.Errorf("%s：密钥泄漏\n  输入: %s\n  输出: %s", c.name, c.in, out)
		}
		// 键名本身要留着 —— 抹掉它 LLM 就不知道是哪个变量出问题了。
		// 断言键名而不是 `name:`，因为 JSON 形态写的是 `"name":`。
		if !strings.Contains(out, c.keyName) {
			t.Errorf("%s：键名 %q 被抹掉了，模型会失去排查线索\n  输出: %s",
				c.name, c.keyName, out)
		}
	}
}

// env 块的反向断言：这些都不该被动。
//
// 误伤同样是 bug —— 把正常的 value 抹掉会让 LLM 分析不了故障，
// 用户也会以为工具坏了。所以「必须脱敏」与「必须不动」两组断言要一起写。
func TestYAMLEnvBlocksDoNotOverRedact(t *testing.T) {
	keep := []struct{ name, in, mustKeep string }{
		{
			name:     "name 不敏感时后面的 value 不该动",
			in:       "- name: LOG_LEVEL\n  value: debug",
			mustKeep: "debug",
		},
		{
			name:     "名字里带敏感词但指的不是密钥（tokens ≠ token）",
			in:       "- name: MAX_TOKENS\n  value: 4096",
			mustKeep: "4096",
		},
		{
			name:     "valueFrom 是引用不是字面密钥，没有 value: 可抹",
			in:       "- name: DB_PASSWORD\n  valueFrom:\n    secretKeyRef:\n      name: my-secret",
			mustKeep: "my-secret",
		},
		{
			name:     "敏感的 name 之后隔了别的键，配对应作废",
			in:       "- name: DB_PASSWORD\n  note: rotated weekly\n  value: not-a-secret",
			mustKeep: "not-a-secret",
		},
		{
			name:     "没有 name 上下文时 value 不该动",
			in:       "config:\n  value: hello",
			mustKeep: "hello",
		},
	}
	for _, c := range keep {
		out, _ := Redact(c.in, nil)
		if !strings.Contains(out, c.mustKeep) {
			t.Errorf("%s：被误伤（%q 不该被抹）\n  输入: %s\n  输出: %s",
				c.name, c.mustKeep, c.in, out)
		}
	}
}

// ---- 不能误伤 ----

// 误伤比漏网更容易被忽略：把正常输出抹掉会让 LLM 分析不了故障，
// 用户也会以为工具坏了。这些都必须原样保留。
func TestDoesNotOverRedact(t *testing.T) {
	keep := []string{
		"user@host:~$ ls -l /etc",
		"total 288",
		"drwxr-xr-x  3 root root    4096 Sep 26 09:12 nginx",
		"-rw-r--r--  1 root root    3042 Sep 12 03:41 passwd",
		"root:x:0:0:root:/root:/bin/bash", // /etc/passwd：口令字段是 x，不是 hash
		"daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin",
		"Active: active (running) since Sat 2026-09-26 09:12:41 CST",
		"Main PID: 1842 (nginx)",
		"Tasks: 5 (limit: 4557)",
		"Memory: 12.4M",
		"CONTAINER ID   IMAGE          COMMAND                  CREATED",
		"192.0.2.44 - - [27/Sep/2026:03:12:41 +0800] \"GET /api/v1/items HTTP/1.1\" 200 5123",
		"kind: Secret",
		"  name: my-app-secret",
		"ssl_certificate_key /etc/letsencrypt/live/x/privkey.pem;",
		"import { createApp } from 'vue'",
		"KEY=VALUE",
		"PATH:=$HOME/bin",
		// 标准 shell 变量：每个 env 输出里都有，抹掉它们毫无意义
		"PWD=/root",
		"OLDPWD=/home/admin",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin",
		"HOME=/root",
		"LANG=en_US.UTF-8",
		// 名字里带敏感词、但指的是别的量
		"MAX_TOKENS=4096",
		"TOKEN_COUNT=10",
		"JWT_TOKEN_TTL=3600",
		"proxy_pass http://backend;",
		"bypass: true",
		"compass_mode=automatic",
		"secretary_name=Alice",
		"monkey=banana",
		"publickey=/root/.ssh/id_rsa.pub",
		"keyboard_layout=us",
		// 普通英文里出现 basic / secret 这类词
		"basic authentication is not supported here",
		"the secret sauce is patience",
	}

	for _, in := range keep {
		out, _ := Redact(in, nil)
		if out != in {
			t.Errorf("不该改动这行\n  输入: %s\n  输出: %s", in, out)
		}
	}
}

// ---- 键名保留 ----

// 抹掉值但保留 `key =` 前缀，LLM 才能理解「这里有个被隐藏的凭据」，
// 而不是看到一段无来由的 [REDACTED]。
func TestKeepsKeyNameForContext(t *testing.T) {
	out, _ := Redact("password=hunter2", nil)
	if !strings.Contains(out, "password") {
		t.Fatalf("应保留键名便于理解上下文，实得 %q", out)
	}
	if !strings.Contains(out, redactedPlaceholder) {
		t.Fatalf("应包含占位符，实得 %q", out)
	}
}

// ---- 边界 ----

func TestRedactEmptyInput(t *testing.T) {
	out, n := Redact("", []string{"whatever"})
	if out != "" || n != 0 {
		t.Fatalf("空输入应原样返回，实得 %q / %d", out, n)
	}
}

// 多条密钥同时出现时要全部抹掉，不能只处理第一条。
func TestRedactsMultipleSecretsInOneText(t *testing.T) {
	in := "password=hunter2\napi_key=abcdef123456\nAKIAIOSFODNN7EXAMPLE"
	out, n := Redact(in, nil)
	for _, leak := range []string{"hunter2", "abcdef123456", "AKIAIOSFODNN7EXAMPLE"} {
		if strings.Contains(out, leak) {
			t.Fatalf("%q 泄漏了\n输出: %s", leak, out)
		}
	}
	if n < 3 {
		t.Fatalf("应至少替换 3 次，实得 %d", n)
	}
}

// ---- 性能 ----
//
// 脱敏跑在**截断之前**（PrepareForLLM 的顺序：先脱敏再截断），
// 所以正则要处理整段输出，上限是 Exec 的 1 MiB 捕获上限。
// 新的赋值模式比原来宽得多（任何 identifier: value 都会被扫到），
// 而且键名不敏感时会从分隔符之后重启扫描 —— 必须确认没有退化。
//
// 实测（i5-12400，256KB 输入，用旧实现的关键部分做对照）：
//
//	输入               旧实现      新实现
//	nginx access log   88.0ms      88.7ms
//	对抗性赋值密集     108ms       137ms
//
// 也就是约 3 MB/s，**新实现与旧实现基本持平**，退化只出现在刻意构造的
// 对抗输入上（+27%），而换来的是一整类真实泄漏被堵住。
//
// 这个数量级不是本次改动引入的 —— 瓶颈在正则引擎的匹配次数，
// 不在自动机大小或重启逻辑。试过两种优化都无收益，已回退：
//   - 去掉冗余的 (?i)、把 {2,64} 放宽成 +  → 无变化（噪声级）
//   - 值内不含 = / : 时跳到匹配末尾        → 无变化（噪声级）
//
// 真正有效的优化方向（未做，因为需要在安全函数里引入「跳过」逻辑，
// 而跳过判错的方向是**静默漏掉凭据**）：先用廉价的 strings.Contains
// 预筛敏感核心词，全不命中就整段跳过赋值扫描。做之前必须先写
// 「预筛不会漏」的测试。
//
// 也不要为了省这点开销把顺序改成「先截断再脱敏」：截断会把跨边界的
// 私钥切成两半，而 PEM 模式要求 BEGIN…END 成对，半截私钥反而不会被
// 任何模式命中 —— 那是把性能问题换成泄漏问题。

func benchmarkRedact(b *testing.B, text string) {
	b.Helper()
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, n := Redact(text, nil); n == 0 {
			b.Fatal("基准数据里应该至少有一处凭据")
		}
	}
}

// 真实形态：一段 256KB 的 nginx access log，末尾夹一条凭据。
// 日志里每行都有 `03:12:41` 这种「像键值对但不是」的片段，
// 正好压到「扫到 → 判定不敏感 → 重启扫描」这条路径。
func BenchmarkRedactNginxLog256K(b *testing.B) {
	var sb strings.Builder
	line := "2026-09-27 03:12:41 web-01 nginx[1842]: 192.0.2.44 - - \"GET /api/v1/items HTTP/1.1\" 200 5123 \"-\" \"Mozilla/5.0\"\n"
	for sb.Len() < 256*1024 {
		sb.WriteString(line)
	}
	sb.WriteString("DB_PASSWORD=hunter2\n")
	benchmarkRedact(b, sb.String())
}

// 对抗形态：大量「外层键名不敏感、值里还嵌着赋值」的片段，
// 逼出重启扫描的最坏情况。
func BenchmarkRedactAdversarialAssignments(b *testing.B) {
	var sb strings.Builder
	for sb.Len() < 256*1024 {
		sb.WriteString("env=AA=BB=CC=DD env=AA=BB=CC=DD\n")
	}
	sb.WriteString("DB_PASSWORD=hunter2\n")
	benchmarkRedact(b, sb.String())
}
