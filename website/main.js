(() => {
  const copy = {
    zh: {
      'nav.github': 'GitHub',
      'hero.brand': 'Ai-Shell',
      'hero.headline': '连上 Linux，用自然语言做运维',
      'hero.lede': '桌面端 SSH 运维台：Agent 排查问题，你也可以自己敲命令——高危变更需确认，凭据永不交给模型。',
      'hero.ctaPrimary': '下载安装包',
      'hero.ctaSecondary': '查看源码',
      'hero.shotAlt': 'Ai-Shell 对话与运维界面截图',
      'lanes.title': '同一控制台，两条工作路径',
      'lanes.lede': '自然语言走 Agent 与工具；像 shell 的一行直接进常驻终端。需要真 TTY 时切到交互终端。',
      'lanes.agentKicker': 'Agent 会话',
      'lanes.agentTitle': '问一句，排查一整台机',
      'lanes.agentBody': '诊断采集、日志与 systemd、状态快照与 diff、变更后校验、文件备份回滚——策略引擎挡住不可逆操作，高危步骤先问你。',
      'lanes.termKicker': '交互终端',
      'lanes.termTitle': '完整 PTY，进程还在',
      'lanes.termBody': 'top、vim、htop 照常用。每条主机会话各有一条 shell，切回去 cwd 与运行中的进程都还在。TUI 定格帧可进模型上下文，但不落历史。',
      'product.shotAlt': 'Ai-Shell 终端与会话界面截图',
      'trust.title': '安全默认，而不是事后补丁',
      'trust.lede': '本机加密凭证库、主机指纹 TOFU、跨主机默认关闭、远端输出按不可信数据处理。你看到的是原文，模型看到的是脱敏后的内容。',
      'trust.item1': '密码与私钥只进本机 vault，LLM 只能看到主机 ID',
      'trust.item2': '硬拒绝破坏性命令与凭据路径；Agent 高危变更需人工批准',
      'trust.item3': '审计日志加密落盘，带哈希链校验与导出',
      'trust.item4': '附图仅当前轮：MIME + 魔数校验，不进历史与审计',
      'start.title': '三分钟跑起来',
      'start.lede': 'Go 1.26+、Node 18+、Wails CLI v2。Windows 也可用 scripts\\build.bat 一键打包。',
      'start.note': '完整说明见仓库 README · MIT 开源',
      'foot.tag': 'LLM 驱动的 Linux 远程运维台'
    },
    en: {
      'nav.github': 'GitHub',
      'hero.brand': 'Ai-Shell',
      'hero.headline': 'Talk to your Linux hosts in plain language',
      'hero.lede': 'A desktop SSH ops console: let the agent diagnose issues, or type shell yourself—high-risk changes need your OK, and credentials never leave the vault.',
      'hero.ctaPrimary': 'Download',
      'hero.ctaSecondary': 'View source',
      'hero.shotAlt': 'Ai-Shell chat and ops console screenshot',
      'lanes.title': 'One console, two ways of working',
      'lanes.lede': 'Natural language goes to the agent and its tools. Shell-like lines go straight into the resident terminal. Need a real TTY? Switch to Interactive terminal.',
      'lanes.agentKicker': 'Agent session',
      'lanes.agentTitle': 'Ask once, inspect the whole host',
      'lanes.agentBody': 'Structured diagnostics, journals and systemd, state snapshots and diffs, post-change verify checks, file backup restore—hard denials stop irreversible damage; risky steps ask you first.',
      'lanes.termKicker': 'Interactive terminal',
      'lanes.termTitle': 'Full PTY. Your processes stay put.',
      'lanes.termBody': 'Run top, vim, htop. Each host session keeps its own shell so cwd and running jobs survive when you switch away. Frozen TUI frames can reach the model as transient context—never session history.',
      'product.shotAlt': 'Ai-Shell terminal and session screenshot',
      'trust.title': 'Secure by default, not bolted on later',
      'trust.lede': 'Encrypted local vault, TOFU host keys, cross-host off by default, remote output treated as untrusted. You see the raw stream; the model sees the sanitized payload.',
      'trust.item1': 'Passwords and keys stay in the vault—the LLM only ever sees opaque host IDs',
      'trust.item2': 'Hard deny for destructive ops and credential paths; agent high-risk actions need approval',
      'trust.item3': 'Encrypted, hash-chained audit trail with verify and export',
      'trust.item4': 'Vision attachments are current-turn only: MIME + magic checks, never history or audit',
      'start.title': 'Up in three minutes',
      'start.lede': 'Go 1.26+, Node 18+, Wails CLI v2. On Windows, scripts\\build.bat builds a self-contained exe.',
      'start.note': 'Full docs in the README · MIT licensed',
      'foot.tag': 'LLM-powered remote Linux ops console'
    }
  };

  const html = document.documentElement;
  const langBtn = document.getElementById('langToggle');
  const top = document.querySelector('.top');

  function applyLang(lang) {
    const dict = copy[lang] || copy.zh;
    html.lang = lang === 'en' ? 'en' : 'zh-CN';
    document.querySelectorAll('[data-i18n]').forEach((el) => {
      const key = el.getAttribute('data-i18n');
      if (dict[key] != null) el.textContent = dict[key];
    });
    document.querySelectorAll('[data-i18n-alt]').forEach((el) => {
      const key = el.getAttribute('data-i18n-alt');
      if (dict[key] != null) el.setAttribute('alt', dict[key]);
    });
    if (langBtn) langBtn.textContent = lang === 'en' ? '中文' : 'EN';
    try {
      localStorage.setItem('aishell-lang', lang);
    } catch { /* ignore */ }
  }

  let lang = 'zh';
  try {
    const saved = localStorage.getItem('aishell-lang');
    if (saved === 'en' || saved === 'zh') lang = saved;
  } catch { /* ignore */ }
  applyLang(lang);

  langBtn?.addEventListener('click', () => {
    lang = lang === 'zh' ? 'en' : 'zh';
    applyLang(lang);
  });

  const onScroll = () => {
    if (!top) return;
    top.classList.toggle('is-scrolled', window.scrollY > 8);
  };
  onScroll();
  window.addEventListener('scroll', onScroll, { passive: true });

  const reduce = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  if (reduce) {
    document.querySelectorAll('.reveal, .reveal-slow').forEach((el) => el.classList.add('is-in'));
    return;
  }

  const io = new IntersectionObserver(
    (entries) => {
      for (const entry of entries) {
        if (entry.isIntersecting) {
          entry.target.classList.add('is-in');
          io.unobserve(entry.target);
        }
      }
    },
    { rootMargin: '0px 0px -8% 0px', threshold: 0.12 }
  );

  document.querySelectorAll('.reveal, .reveal-slow').forEach((el, i) => {
    if (el.closest('.hero')) {
      // Stagger hero copy without waiting for scroll.
      window.setTimeout(() => el.classList.add('is-in'), 80 + i * 90);
    } else {
      io.observe(el);
    }
  });
})();
