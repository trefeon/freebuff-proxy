/**
 * Minimal dependency-free i18n for the dashboard (issue #156).
 *
 * Keys ARE the English source strings: the `en` dictionary is the identity
 * map, so unwrapped or unknown keys degrade to readable English instead of
 * breaking. `zh` carries the simplified-Chinese translations.
 *
 * Usage in components:
 *   import { tr, setLocale } from '../lib/i18n.js';
 *   {$tr('Overview')}          // reactive — re-renders on locale switch
 *   {$tr('banned until {time}', { time })}
 *
 * Locale resolution: localStorage('fp_locale') > navigator.language > 'en'.
 */

const en = {}; // identity — keys resolve to themselves

const zh = {
	// Navigation
	'Overview': '仪表盘',
	'Tokens': '令牌',
	'Models': '模型',
	'Config': '配置',
	'Logs': '日志',
	'Setup': '接入',
	'Language': '语言',

	// Session-expired banner
	'Session expired': '会话已过期',
	'Your session has ended. Sign in again to continue using the dashboard.':
		'您的会话已结束，请重新登录以继续使用仪表盘。',
	'Log in': '登录',
	'Dismiss session expired notice': '关闭会话过期提示',

	// Overview
	'Live proxy status and token pool telemetry': '实时代理状态与令牌池遥测',
	'Overview unavailable': '仪表盘不可用',
	'Retry': '重试',
	'Bridge mode relays upstream tokens per client request':
		'桥接模式按客户端请求转发上游令牌',
	'active bridge client(s)': '个活跃桥接客户端',
	'Session pools and quota tracking are client-scoped.': '会话池与配额跟踪按客户端隔离。',
	'No upstream tokens configured': '未配置上游令牌',
	'Add tokens to AUTH_TOKENS in Config to start the pooled relay.':
		'请在“配置”页向 AUTH_TOKENS 添加令牌以启动池化转发。',
	'Go to Config': '前往配置',
	'Pool total': '池总数',
	'Busy': '忙碌',
	'tokens with active runs': '正在执行运行的令牌数',
	'Cooldown': '冷却中',
	'Banned': '已封禁',
	'critical risk': '高风险',
	'Requests today': '今日请求数',
	'Token risk': '令牌风险',
	'banned — appeal required': '已封禁 — 需申诉',
	'banned (temporary)': '已封禁（临时）',
	'banned until {time}': '封禁至 {time}',
	'msgs today': '条消息（今日）',
	'msgs 24h': '条消息（24小时）',
	'All tokens healthy — no risk flags.': '所有令牌正常 — 无风险标记。',

	// Tokens
	'Upstream credentials, device login, client API keys, and per-token session quotas':
		'上游凭据、设备登录、客户端 API 密钥与各令牌的会话配额',
	'Authorizing…': '授权中…',
	'Device Login': '设备登录',
	'Generating…': '生成中…',
	'Generate API Key': '生成 API 密钥',
	'Copy link': '复制链接',
	'Open': '打开',
	'Add Token to Pool': '向池中添加令牌',
	'Paste a FreeBuff auth token (cb_…) to add it to the shared pool and save it to .env. Adding burns no quota.':
		'粘贴 FreeBuff 认证令牌（cb_…）加入共享池并保存到 .env。添加不会消耗配额。',
	'Token': '令牌',
	'Valid format': '格式正确',
	'Invalid format': '格式错误',
	'Format: cb_…': '格式：cb_…',
	'Token must match cb_… with at least 20 characters': '令牌须符合 cb_… 格式且至少 20 个字符',
	'Add Token': '添加令牌',
	'Client API Keys': '客户端 API 密钥',
	'sk-fb-… credentials for clients (omp, curl) to authenticate against this proxy. Stored in the API_KEYS line of .env.':
		'sk-fb-… 客户端凭据（omp、curl 等）用于访问本代理，保存在 .env 的 API_KEYS 行。',
	'New key:': '新密钥：',
	'Copy': '复制',
	'Pool Tokens': '池内令牌',
	'{count} pooled token(s)': '共 {count} 个池内令牌',
	'Could not load tokens': '无法加载令牌',
	'No tokens in pool': '池内暂无令牌',
	'Add one above or use Device Login to generate credentials via browser.':
		'在上方添加，或使用设备登录通过浏览器生成凭据。',
	'Status': '状态',
	'Instance': '实例',
	'Actions': '操作',
	'locked': '已锁定',
	'cooldown': '冷却中',
	'leased': '使用中',
	'queued': '排队中',
	'banned': '已封禁',
	'idle': '空闲',
	'expiring': '即将到期',
	'Clear': '解除',
	'Unlock': '解锁',
	'Lock': '锁定',
	'Remove': '移除',
	'Clear cooldown for token {idx}? Only do this if the lock is stale.':
		'清除令牌 {idx} 的冷却？仅在该锁定已过期时执行。',
	'Unlock token {idx}?': '解锁令牌 {idx}？',
	'Lock token {idx}?': '锁定令牌 {idx}？',
	'Remove token {idx} from the pool and .env?': '从池和 .env 中移除令牌 {idx}？',
	'Active Session:': '活跃会话：',
	'remaining': '剩余',
	'Session quotas': '会话配额',
	'(remaining {count})': '（剩余 {count}）',
	'entitled': '含额度',
	'reset': '重置于',
	'No quota data available for this session.': '该会话暂无配额数据。',
	'Token added successfully': '令牌添加成功',
	'Failed to add token': '添加令牌失败',
	'Network error adding token': '添加令牌时网络错误',
	'Action completed': '操作完成',
	'Action failed': '操作失败',
	'Network error executing action': '执行操作时网络错误',
	'Generated & saved client API key': '客户端 API 密钥已生成并保存',
	'Failed to save client API key': '保存客户端 API 密钥失败',
	'Network error generating client key': '生成客户端密钥时网络错误',
	'Failed to fetch tokens': '获取令牌失败',
	'Starting headless login flow…': '正在启动无头登录流程…',
	'Open this URL in your browser to sign in:': '请在浏览器中打开此链接完成登录：',
	'Token #{idx} added to pool and saved to .env.': '令牌 #{idx} 已加入池并保存到 .env。',
	'Login failed: {message}': '登录失败：{message}',
	'unknown error': '未知错误',
	'Failed to start login wizard.': '启动登录向导失败。',

	// Login
	'Admin token': '管理员令牌',
	'Enter admin token': '输入管理员令牌',
	'Sign in': '登录',
	'Enter your admin token to access the dashboard.': '输入管理员令牌以访问仪表盘。',
	'Set ADMIN_TOKEN in your .env file to configure access.': '在 .env 文件中设置 ADMIN_TOKEN 以配置访问。',

	// Models
	'Served model catalog with upstream agent bindings and client aliases.':
		'已服务模型目录，含上游代理绑定与客户端别名。',
	'Failed to load models': '加载模型失败',
	'Served Models': '已服务模型',
	'{count} registered · {agents} agents': '{count} 个注册模型 · {agents} 个代理',
	'Model Catalog': '模型目录',
	'Model ID': '模型 ID',
	'Served': '已服务',
	'Agent': '代理绑定',
	'Client Aliases': '客户端别名',
	'served': '服务中',
	'unbound': '未绑定',
	'No models registered': '无注册模型',
	'The model registry is empty. Add model-to-agent mappings in the gateway config and reload.':
		'模型注册表为空。请在网关配置中添加模型到代理的映射后重新加载。',

	// Config
	'Runtime .env editor — Save writes the file and reloads the running proxy.':
		'.env 运行时编辑器 — 保存将写入文件并热加载代理。',
	'Reload': '重新加载',
	'Save': '保存',
	'.env Editor': '.env 编辑器',
	'Edit environment variables. Save validates server-side and reloads; rejected writes are rolled back.':
		'编辑环境变量。保存在服务端校验并热加载；被拒绝的写入自动回滚。',
	'env loaded': '已加载 .env',
	'no env file': '无 .env 文件',
	'{count} changed': '{count} 处修改',
	'Environment file content': '环境文件内容',
	'{count} validation error(s):': '{count} 个校验错误：',
	'… and {count} more': '… 还有 {count} 处',
	'lines': '行',
	'keys': '个键',
	'saved {time}': '保存于 {time}',
	'Validate': '校验',
	'Changes take effect after save.': '更改保存后生效。',
	'saves from the keyboard': '快捷键保存',
	'Effective Configuration': '生效配置',
	'Read-only view of the running configuration. Secret values are masked.':
		'运行配置的只读视图，敏感值已脱敏。',
	'Key': '键',
	'Value': '值',
	'secret': '敏感',
	'copy': '复制',
	'No effective configuration': '暂无生效配置',
	'Start the proxy to populate this view.': '启动代理后将显示此处内容。',
	'Refresh': '刷新',
	'Dismiss alert': '关闭提示',
	'Save the .env file and reload the proxy with these changes?':
		'保存 .env 文件并以这些更改重载代理？',
	'Configuration is empty — nothing to save.': '配置为空 — 无可保存内容。',
	'Configuration is valid — {count} key(s) parsed.': '配置有效 — 解析出 {count} 个键。',
	'Configuration invalid ({count}): {detail}': '配置无效（{count} 处）：{detail}',
	'Configuration saved and reloaded.': '配置已保存并重载。',
	'Save failed': '保存失败',
	'Network error saving configuration': '保存配置时网络错误',
	'Failed to fetch configuration': '获取配置失败',

	// Logs
	'Structured entries from the in-memory ring buffer (200 max, newest first), filtered by level and message.':
		'内存环形缓冲中的结构化日志（最多 200 条，最新在前），可按级别与消息过滤。',
	'Log level': '日志级别',
	'All levels': '全部级别',
	'Debug': '调试',
	'Info': '信息',
	'Warn': '警告',
	'Error': '错误',
	'Filter by message': '按消息过滤',
	'Filter message…': '过滤消息…',
	'Auto {state}': '自动 {state}',
	'on': '开',
	'off': '关',
	'Clear filters': '清除筛选',
	'Refresh': '刷新',
	'Could not load log entries': '无法加载日志',
	'Could not load log entries: {reason}': '无法加载日志：{reason}',
	'Log ring disabled': '日志环未启用',
	'The server was started without an active logring handler, so no log entries are available.':
		'服务器启动时未挂载日志环处理器，因此没有可用日志。',
	'No matching log entries': '无匹配日志',
	'No log entries matched your level or message filter.': '没有符合级别或消息筛选条件的日志。',
	'The log ring is empty — entries will appear here as the proxy logs activity.':
		'日志环为空 — 代理产生活动后日志将显示在此处。',
	'Prev': '上一页',
	'Next': '下一页',
	'Page {current} / {total}': '第 {current} / {total} 页',

	// Setup
	"Client configuration for AI coding tools — copy a block into your tool's config.":
		'AI 编程工具的客户端配置 — 将对应代码块复制到工具配置中即可。',
	'Failed to load setup data': '加载接入数据失败',
	'Loading setup data': '正在加载接入数据',
	'Mode': '模式',
	'How clients authenticate to this gateway': '客户端如何认证到本网关',
	'Bridge mode — no token pool. Each client sends its own FreeBuff token; the proxy relays the Authorization header straight upstream.':
		'桥接模式 — 无令牌池。每个客户端使用自己的 FreeBuff 令牌，代理将 Authorization 头直接转发给上游。',
	'Pooled mode — the proxy holds the upstream AUTH_TOKENS and selects one per request; clients authenticate with any key.':
		'池化模式 — 代理持有上游 AUTH_TOKENS 并按请求选取；客户端使用任意密钥认证。',
	'Client API Key': '客户端 API 密钥',
	'The key embedded in every snippet below.': '下方所有代码片段中内嵌的密钥。',
	'Quick Start': '快速开始',
	'Base URL': '基础地址',
	'OpenAI-compatible endpoint — same for every tool.': 'OpenAI 兼容端点 — 所有工具通用。',
	'Model IDs available to clients': '客户端可用的模型 ID',

	// Footer / banners
	'Update available': '有可用更新',
	'update to v{version}': '更新至 v{version}',

	// Security banner
	'Security Warning': '安全警告',
	'You are using the default admin password (123456). Change it immediately to secure this instance.':
		'您正在使用默认管理员密码（123456）。请立即修改以保护此实例。',
	'Change Password': '修改密码',

	// Change-password modal
	'Change Admin Password': '修改管理员密码',
	'Update the master administrative password': '更新管理员主密码',
	'Close dialog backdrop': '关闭对话框遮罩',
	'Close modal': '关闭弹窗',
	'Current Password': '当前密码',
	'Enter current password (default: 123456)': '输入当前密码（默认：123456）',
	'New Password': '新密码',
	'Minimum 6 characters': '至少 6 个字符',
	'Enter secure new password': '输入新的安全密码',
	'Confirm New Password': '确认新密码',
	'Re-enter new password': '再次输入新密码',
	'Cancel': '取消',
	'Update Password': '更新密码',
	'Please enter your current password.': '请输入当前密码。',
	'New password must be at least 6 characters.': '新密码长度至少 6 位。',
	'New password cannot be the default password (123456).': '新密码不能是默认密码（123456）。',
	'New passwords do not match.': '两次输入的新密码不一致。',
	'Admin password updated successfully!': '管理员密码更新成功！',
	'Failed to update password.': '密码更新失败。',
	'Could not update password. Check connection.': '无法更新密码，请检查连接。',
	// Late additions (error fallbacks + setup field label)
	'Could not reach the proxy API. Check that the server is running.':
		'无法连接代理 API，请确认服务器正在运行。',
	'Invalid password.': '密码错误。',
	'Could not reach the server. Check the connection and try again.':
		'无法连接服务器，请检查网络后重试。',
	'Network error: {message}': '网络错误：{message}',
	'and {count} more': '还有 {count} 处',
	'Client API key': '客户端 API 密钥',
};

const dictionaries = { en, zh };

import { writable, derived } from 'svelte/store';

function initialLocale() {
	if (typeof window === 'undefined') return 'en';
	try {
		const saved = window.localStorage.getItem('fp_locale');
		if (saved === 'zh' || saved === 'en') return saved;
	} catch {
		// storage blocked — fall through to browser detection
	}
	return typeof navigator !== 'undefined' && navigator.language?.startsWith('zh')
		? 'zh'
		: 'en';
}

/** Reactive locale store; persisted to localStorage on change. */
export const locale = writable(initialLocale());

locale.subscribe((value) => {
	if (typeof window === 'undefined') return;
	try {
		window.localStorage.setItem('fp_locale', value);
	} catch {
		// storage blocked — in-memory locale still works
	}
});

/** Switch the UI language ('en' | 'zh'). */
export function setLocale(next) {
	locale.set(next === 'zh' ? 'zh' : 'en');
}

function interpolate(template, params) {
	if (!params) return template;
	return template.replace(/\{(\w+)\}/g, (match, name) =>
		params[name] !== undefined ? String(params[name]) : match
	);
}

/**
 * Reactive translator store: `$tr('key', params)` in components.
 * Falls back to the raw English key when a translation is missing, so the
 * dashboard never renders blanks.
 */
export const tr = derived(locale, (loc) => {
	const dict = dictionaries[loc] || {};
	return (key, params) => {
		const template = dict[key] ?? key;
		return interpolate(template, params);
	};
});
