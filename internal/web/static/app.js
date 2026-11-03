'use strict';
/* drama-hub 管理面脚本。
 *
 * 一套通用请求/渲染小工具 + 两个页面的 init，靠 body[data-page] 分派。
 * 只用原生 DOM API、不引框架、没有构建步骤（ADR-0002）；页面数据一律来自
 * /api/ 下的 JSON 端点，渲染结果与 API 返回一一对应。 */

// ---- 通用 ----

// ---- 管理令牌 ----
// 写操作（非 GET/HEAD/OPTIONS）要带 X-Admin-Token（env DRAMA_HUB_ADMIN_TOKEN）。
// 令牌只存在 sessionStorage：关掉标签页就没了 —— 改配置是低频动作，不值得长期留在浏览器。
const TOKEN_KEY = 'drama-hub-admin-token';
const TOKEN_HEADER = 'X-Admin-Token';

/** 当前管理令牌：以顶栏输入框为准（刚打完字、还没触发 change 时也拿得到）。 */
function adminToken() {
  const input = document.getElementById('admin-token');
  if (input && input.value.trim() !== '') return input.value.trim();
  return sessionStorage.getItem(TOKEN_KEY) || '';
}

/** 顶栏令牌框：回填上次输入，改动即记住（清空 = 忘掉）。 */
function initAdminToken() {
  const input = document.getElementById('admin-token');
  if (!input) return;
  input.value = sessionStorage.getItem(TOKEN_KEY) || '';
  input.addEventListener('change', () => {
    const value = input.value.trim();
    input.value = value;
    if (value === '') sessionStorage.removeItem(TOKEN_KEY);
    else sessionStorage.setItem(TOKEN_KEY, value);
  });
}

/** 请求管理 API 并返回解析后的 JSON（204 返回 null）。非 2xx 抛 Error（带服务端 message）。 */
async function api(method, path, body) {
  const init = { method, headers: { Accept: 'application/json' } };
  if (method !== 'GET') init.headers[TOKEN_HEADER] = adminToken();
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  const res = await fetch(path, init);
  const text = await res.text();
  let payload = null;
  if (text !== '') {
    try {
      payload = JSON.parse(text);
    } catch {
      payload = null;
    }
  }
  if (!res.ok) {
    const message = payload && payload.message ? payload.message : res.status + ' ' + res.statusText;
    // status 挂在错误上：调用方要区分 409（已在运行）与其它失败，只能靠码不靠文案。
    const err = new Error(message);
    err.status = res.status;
    throw err;
  }
  return payload;
}

/** 建节点：props 里 text 走 textContent，on* 走事件，其余走 setAttribute。 */
function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (value === undefined || value === null) continue;
    if (key === 'text') node.textContent = value;
    else if (key === 'class') node.className = value;
    else if (key.startsWith('on')) node.addEventListener(key.slice(2), value);
    else node.setAttribute(key, String(value));
  }
  for (const child of children.flat()) {
    if (child === undefined || child === null) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

/** 在页面的 #notice 上显示一条提示（busy / ok / error）。 */
function say(text, kind) {
  const box = document.getElementById('notice');
  if (!box) return;
  box.textContent = text;
  box.dataset.kind = kind || '';
}

/** 统一的失败出口：错误文案来自响应体，直接显示。 */
function fail(err) {
  say(err && err.message ? err.message : String(err), 'error');
}

/** 把 UTC RFC3339 文本按浏览器本地时区显示；空值显示破折号。 */
function fmtTime(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const p = (n) => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) +
    ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
}

// ---- 剧目页 ----

function initDramas() {
  const form = document.getElementById('drama-form');
  const input = document.getElementById('series-id');
  const syncBtn = document.getElementById('sync-btn');
  const tbody = document.getElementById('dramas');
  const empty = document.getElementById('dramas-empty');

  function render(dramas) {
    tbody.replaceChildren();
    for (const drama of dramas) {
      const deleteBtn = el('button', {
        type: 'button', class: 'btn ghost', text: '删除',
        onclick: () => removeDrama(drama),
      });
      tbody.append(el('tr', {},
        el('td', {}, el('div', { class: 'title-cell' },
          el('span', { text: drama.title }),
          el('span', { class: 'sub', text: drama.provider + ':' + drama.provider_id }))),
        el('td', { class: 'num', text: drama.year === 0 ? '—' : String(drama.year) }),
        el('td', { class: 'num', text: drama.episodes + ' / ' + drama.episode_total }),
        el('td', { class: 'num', text: String(drama.emby_items) }),
        el('td', { class: 'mono', text: fmtTime(drama.sync_at) }),
        el('td', { class: 'num' }, deleteBtn)));
    }
    empty.hidden = dramas.length !== 0;
  }

  async function reload() {
    const data = await api('GET', '/api/dramas');
    render(data.dramas);
  }

  async function removeDrama(drama) {
    const ok = window.confirm('删除《' + drama.title + '》？它的 ' + drama.episodes +
      ' 集会一并删除。');
    if (!ok) return;
    try {
      await api('DELETE', '/api/dramas/' + drama.id);
      await reload();
      say('已删除《' + drama.title + '》', 'ok');
    } catch (err) {
      fail(err);
    }
  }

  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    const seriesId = input.value.trim();
    if (seriesId === '') return;
    syncBtn.disabled = true;
    say('正在同步 ' + seriesId + ' …', 'busy');
    try {
      const data = await api('POST', '/api/dramas', { series_id: seriesId });
      render(data.dramas);
      input.value = '';
      say('同步完成，库里共 ' + data.dramas.length + ' 部剧', 'ok');
    } catch (err) {
      fail(err);
    } finally {
      syncBtn.disabled = false;
    }
  });

  reload().catch(fail);
}

// ---- 同步与缓存看板 ----

function initStatus() {
  const totalsBox = document.getElementById('totals');
  const sourcesBody = document.getElementById('sources');
  const configBody = document.getElementById('config');
  const form = document.getElementById('settings-form');
  const saveBtn = document.getElementById('settings-save');
  const hint = document.getElementById('settings-hint');
  const jellyfinBody = document.getElementById('jellyfin');
  const jellyfinBtn = document.getElementById('jellyfin-sync');
  const jellyfinHint = document.getElementById('jellyfin-hint');

  // 表单字段 ↔ settings 键。页面不另写一份校验：值原样交给 PATCH，越界由服务端的
  // 注册表拒（400），这里只显示服务端给的原因。
  const fields = {
    strm_prefix: 'set-strm-prefix',
    cache_max_mb: 'set-cache-max-mb',
    cache_lru_days: 'set-cache-lru-days',
    merge_chunk_size: 'set-merge-chunk-size',
    refresh_interval_sec: 'set-refresh-interval',
  };

  // WebDAV 凭据只写不读：/api/status 只报「是否已设置」，不回传值（凭据不该被多抄一份
  // 到浏览器），所以这两项没有基线，留空 = 不改。
  const credentials = {
    webdav_user: 'set-webdav-user',
    webdav_pass: 'set-webdav-pass',
  };

  // Jellyfin 的四项可改配置：输入框 id、settings 键、/api/status 里的字段名三者不同名，
  // 所以逐项列出来（不靠命名约定拼字符串）。它们不在 config 块里，基线单独存一份。
  const jellyfinFields = [
    { key: 'jellyfin_url', id: 'set-jellyfin-url', view: 'url' },
    { key: 'jellyfin_library_root', id: 'set-jellyfin-root', view: 'library_root' },
    { key: 'jellyfin_library_prefix', id: 'set-jellyfin-prefix', view: 'library_prefix' },
    { key: 'jellyfin_timeout_sec', id: 'set-jellyfin-timeout', view: 'timeout_sec' },
  ];
  let jellyfinBaseline = {};

  let baseline = {}; // 上次读到的值：提交时只发改过的项（清空 strm_prefix 也算改）
  let prefilled = false; // 表单只在首次加载与保存成功后回填，不冲掉正在打的值
  let adminWritable = false; // 服务端有没有配 DRAMA_HUB_ADMIN_TOKEN

  function stat(label, value, small) {
    return el('div', {},
      el('dt', { text: label }),
      el('dd', { class: small ? 'small' : null, text: value }));
  }

  function row(name, value, timing, note, mono) {
    return el('tr', {},
      el('td', { text: name }),
      el('td', { class: mono ? 'mono' : null }, value instanceof Node ? value : String(value)),
      el('td', { class: 'hint', text: timing }),
      el('td', { class: 'hint', text: note }));
  }

  function flag(set) {
    return el('span', { class: set ? 'tag' : 'tag warn', text: set ? '已设置' : '未设置' });
  }

  // sourceRow 渲染一个播放源：调度状态（上次运行/上次成功/下次/上次错误）来自
  // /api/status 的调度字段，说明与配置来自 settings 现状。
  function sourceRow(src) {
    const state = src.running
      ? el('span', { class: 'tag', text: '正在刷新' })
      : src.enabled
        ? el('span', { class: 'tag', text: '已启用' })
        : el('span', { class: 'tag warn', text: '已停用' });
    // base_url / request_timeout_sec 是启动时注入插件的构造期配置：显示正在生效的
    // 值，并明说 settings 现值是否还要重启才生效。
    const bootNote = 'base_url 生效值 ' +
      (src.base_url_effective === '' ? '插件内置默认' : src.base_url_effective) +
      '，请求超时生效值 ' + src.request_timeout_sec_effective + ' 秒（重启后生效），间隔 ' +
      src.interval_sec + ' 秒' +
      (src.restart_required
        ? '；settings 现值（base_url ' + (src.base_url === '' ? '空' : src.base_url) +
          ' / 超时 ' + src.request_timeout_sec + ' 秒）重启后生效'
        : '');
    const note = src.reason !== '' ? src.reason : bootNote;
    const error = src.last_error === ''
      ? '—'
      : el('span', { class: 'mono', text: src.last_error });
    return el('tr', {},
      el('td', { text: src.label === src.name ? src.name : src.name + '（' + src.label + '）' }),
      el('td', {}, state),
      el('td', { class: 'mono', text: fmtTime(src.last_run_at) }),
      el('td', { class: 'mono', text: fmtTime(src.last_ok_at) }),
      el('td', { class: 'mono', text: fmtTime(src.next_due_at) }),
      el('td', { class: 'hint' }, el('div', { text: note }), error),
      el('td', { class: 'num' },
        el('button', {
          type: 'button', class: 'btn ghost', text: '立即刷新',
          disabled: src.running || !src.enabled,
          onclick: (event) => refreshSource(src, event.currentTarget),
        })));
  }

  // refreshSource 打手动触发端点：202 = 一轮已派出去，409 = 该源正在跑，
  // 401 = 没带对管理令牌，404 = 没有这个源或已停用。结果都回读 /api/status
  // （不猜这一轮的结局）。
  async function refreshSource(src, button) {
    button.disabled = true;
    say('已请求刷新 ' + src.name + ' …', 'busy');
    try {
      await api('POST', '/api/refresh', { source: src.name });
      say('已派出 ' + src.name + ' 的一轮刷新，结果看「上次运行」与「上次错误」', 'ok');
    } catch (err) {
      fail(err);
    }
    await load();
  }

  // renderConfig 渲染「运行配置」表：每行都带生效时机（MKACG-617 的生效性矩阵）。
  function renderConfig(c) {
    configBody.replaceChildren(
      row('strm 前缀', c.strm_prefix === '' ? '空（按请求 Host 推导）' : c.strm_prefix, '即时',
        'strm 内 URL 前缀；空 = 用访问本服务的 Host 拼绝对地址，填了就用填的（写入时去掉尾 /）', true),
      row('缓存目录', c.cache_dir, '即时',
        'cache_dir，海报与分片落 <cache_dir>/{cover,seg}，写入中的临时文件在 tmp/', true),
      row('缓存上限', c.cache_max_mb + ' MB', '≤5 分钟',
        'cache_max_mb，0 = 不按大小淘汰；下一轮淘汰生效'),
      row('缓存保留', c.cache_lru_days + ' 天', '≤5 分钟',
        'cache_lru_days，超过 N 天未命中的条目删除'),
      row('合并分块集数', c.merge_chunk_size + ' 集 / 对外集', '下次重建',
        'merge_chunk_size，每 N 集并成 1 条对外剧集；改完下一次同步或首播回填重建派生表时生效，旧 id 随即 404'),
      row('刷新间隔', c.refresh_interval_sec + ' 秒', '≤60 秒',
        'refresh_interval_sec，一轮到期 = 上次运行 + 本值'),
      row('每轮存量部数', String(c.refresh_batch), '即时', 'refresh_batch，0 = 关闭存量轮转'),
      row('单轮预算', c.refresh_timeout_sec + ' 秒', '即时',
        'refresh_timeout_sec，超时后下一轮按游标续跑'),
      row('出流签名密钥', flag(c.stream_secret_set), '即时',
        'stream_secret（ADR-0003）；换了之后所有旧 URL 失效'),
      row('WebDAV 用户', flag(c.webdav_user_set), '即时', 'webdav_user'),
      row('WebDAV 密码', flag(c.webdav_pass_set), '即时', 'webdav_pass'));
  }

  // fillSettings 把库里现在的值填进表单，并记成基线（提交时只发改过的项）。
  function fillSettings(c) {
    baseline = {};
    for (const [key, id] of Object.entries(fields)) {
      baseline[key] = String(c[key]);
      document.getElementById(id).value = baseline[key];
    }
    setCredentialPlaceholders(c);
  }

  // setCredentialPlaceholders 把「已设置 / 未设置」写在凭据输入框的占位符里：
  // 值本身不回显（/api/status 只给 *_set 标志）。
  function setCredentialPlaceholders(c) {
    document.getElementById('set-webdav-user').value = '';
    document.getElementById('set-webdav-user').placeholder =
      c.webdav_user_set ? '已设置，留空不改' : '未设置（/webdav 恒 401）';
    document.getElementById('set-webdav-pass').value = '';
    document.getElementById('set-webdav-pass').placeholder =
      c.webdav_pass_set ? '已设置，留空不改' : '未设置（/webdav 恒 401）';
  }

  // syncHint 说明「现在能不能写」：没配 env 令牌就写不了；配了但要先在顶栏填上。
  function syncHint() {
    if (!adminWritable) {
      saveBtn.disabled = true;
      jellyfinBtn.disabled = true;
      hint.textContent = '未设置 DRAMA_HUB_ADMIN_TOKEN：后台改配置不可用（所有写操作都会 401）。' +
        '在服务端加上这个 env 再重启，然后用这个表单改配置。';
      jellyfinHint.textContent = '同步媒体库是写操作，同样需要 DRAMA_HUB_ADMIN_TOKEN。';
      return;
    }
    saveBtn.disabled = adminToken() === '';
    jellyfinBtn.disabled = adminToken() === '';
    jellyfinHint.textContent = adminToken() === ''
      ? '先在顶栏「管理令牌」里填 DRAMA_HUB_ADMIN_TOKEN，再点「同步媒体库」（令牌只存在本标签页）。'
      : '只增不删：分类消失只在这里列成「孤儿库」。';
    hint.textContent = adminToken() === ''
      ? '先在顶栏「管理令牌」里填 DRAMA_HUB_ADMIN_TOKEN，再保存（令牌只存在本标签页）。'
      : '写 PATCH /api/settings；非法值整批拒绝（400）且库里不动；密码不回显，留空表示不改。';
  }

  // ---- 媒体库同步的进度轮询 ----
  // 202 不带结果，所以点完只能轮询 /api/status 直到这一轮收尾。轮询期间每轮都把
  // 「阶段 + 已完成/总数 + 本轮计数」重画进表格；收尾后停下并给一句结果文案。
  // 页面刷新后如果那一轮还在跑，load() 也会接上轮询，不需要重新点。
  const POLL_MS = 1000;
  const POLL_MAX_MS = 30 * 60 * 1000; // 整轮预算可以很长（58 分类 × 超时），但页面不无限转
  let pollTimer = null;
  let pollDeadline = 0;
  // watchingSeq 是「我点出来的那一轮」的序号：0 = 只是在显示进度，不认领结果。
  let watchingSeq = 0;

  function stopPoll() {
    if (pollTimer !== null) {
      clearTimeout(pollTimer);
      pollTimer = null;
    }
  }

  // poll 排下一次轮询。它只是「再读一次 /api/status」，收尾判定在 afterRender 里。
  //
  // 上限只是防「页面无限转」：正常一轮的时长由服务端的整轮预算决定（可以到分钟级），
  // 所以 30 分钟只是页面侧的保护，不是服务端的运行上限。
  function poll() {
    if (pollTimer !== null) return;
    if (pollDeadline === 0) pollDeadline = Date.now() + POLL_MAX_MS;
    if (Date.now() > pollDeadline) {
      stopPoll();
      say('同步还在跑，但页面已停止轮询（超过 ' + POLL_MAX_MS / 60000 + ' 分钟）：' +
        '仍可看 /api/status 的 jellyfin 块', 'busy');
      return;
    }
    pollTimer = setTimeout(async () => {
      pollTimer = null;
      try {
        const status = await api('GET', '/api/status');
        applyStatus(status);
        afterRender(status.jellyfin);
      } catch (err) {
        fail(err);
        stopPoll();
      }
    }, POLL_MS);
  }

  // resultKind 给结果文案选色调：整轮失败 / 有被拒分类 = error，其余 ok。
  function resultKind(j) {
    return j.last_error !== '' ? 'error' : 'ok';
  }

  // phaseText 把「当前阶段 + 已完成/总数」写成一行进度文本。
  function phaseText(j) {
    if (!j.phase) return '—';
    return j.phase_total > 0 ? j.phase + ' ' + j.phase_done + '/' + j.phase_total : j.phase;
  }

  // resultText 给一句人能直接读的结果文案（成功 N 个 / 失败 M 个及原因 / 耗时）。
  function resultText(j) {
    if (!j.last_run_at) return '—';
    const parts = ['新建 ' + j.created + ' 个', '已存在 ' + j.unchanged + ' 个'];
    if (j.failed.length > 0) parts.push('被拒 ' + j.failed.length + ' 个（' + j.failed.join('、') + '）');
    if (j.uncertain > 0) parts.push('结果未知 ' + j.uncertain + ' 个');
    parts.push('耗时 ' + fmtDuration(j.duration_ms));
    const text = parts.join('、');
    // 整轮失败（Jellyfin 不可达等）时 last_error 是唯一的原因来源，别让它只出现在下面一行。
    return j.last_error === '' ? text : text + '；' + j.last_error;
  }

  // fmtDuration 把毫秒写成「1.2 秒」/「2 分 05 秒」。
  function fmtDuration(ms) {
    if (!ms) return '—';
    if (ms < 60000) return (ms / 1000).toFixed(1) + ' 秒';
    const total = Math.round(ms / 1000);
    const p = (n) => String(n).padStart(2, '0');
    return Math.floor(total / 60) + ' 分 ' + p(total % 60) + ' 秒';
  }

  // renderJellyfin 渲染媒体库现状：配置四项 + 上次结果 + 跑着时的进度。
  // 孤儿库只报告，prune 不进 UI（破坏性动作只走 API，少一个误点路径）。
  function renderJellyfin(j) {
    const orphans = j.orphans.length === 0 ? '—' : j.orphans.join('、');
    const conflicts = j.conflicts.length === 0 ? '—' : j.conflicts.join('、');
    const missing = j.missing.length === 0 ? '' : '（缺 ' + j.missing.join('、') + '）';
    const state = j.running
      ? el('span', { class: 'tag', text: '正在同步 · ' + phaseText(j) })
      : j.configured
        ? el('span', { class: 'tag', text: '已启用' })
        : el('span', { class: 'tag warn', text: '未配置' });
    const rows = [
      row('状态', el('span', {}, state, missing), '即时',
        '地址 / API key / 媒体库根三项都非空才启用；留空 = 整段关闭'),
      row('地址', j.url === '' ? '—' : j.url, '即时', 'jellyfin_url', true),
      row('媒体库根', j.library_root === '' ? '—' : j.library_root, '即时',
        'jellyfin_library_root，必须是 Jellyfin 容器内看到的路径', true),
      row('库名前缀', j.library_prefix, '即时',
        'jellyfin_library_prefix，只操作带这个前缀的库'),
      row('API key', flag(j.api_key_set), '即时', 'jellyfin_api_key（只写不读，状态面不返回值）'),
      row('单次超时', j.timeout_sec + ' 秒', '即时', 'jellyfin_timeout_sec，每次出站请求一个超时'),
    ];
    // 跑着的时候先给进度：这是「点完之后看见发生了什么」的主路径。
    if (j.running) {
      rows.push(row('本次进度', phaseText(j), '—',
        '当前阶段：列分类 → 比对现有库 → 创建媒体库 →（prune 时）删除孤儿库 → 触发扫描'));
      rows.push(row('本次已处理', '新建 ' + j.created + ' 个' +
        (j.uncertain > 0 ? '、结果未知 ' + j.uncertain + ' 个' : '') +
        (j.failed.length > 0 ? '、被拒 ' + j.failed.length + ' 个' : ''), '—',
        '开始于 ' + fmtTime(j.started_at) + '，计数随轮询实时变化'));
    }
    rows.push(
      row('上次同步', fmtTime(j.last_run_at), '—',
        '页面上常驻，不需要点开或刷新；状态在内存里，重启后回到「未运行」'),
      row('上次结果', resultText(j), '—',
        'created / unchanged / failed / uncertain 与耗时',
        true),
      row('上次耗时', fmtDuration(j.duration_ms), '—', 'duration_ms'),
      row('上次成功', fmtTime(j.last_ok_at), '—', '孤儿与同名冲突不算失败，也会推进这一项'),
      row('上次错误', j.last_error === '' ? '—' : el('span', { class: 'mono', text: j.last_error }), '—',
        'Jellyfin 不可用只影响这一段：同步、WebDAV、播放都不受影响'),
      row('上次新建', String(j.created), '—', '这一轮建了几个库；0 = 幂等命中'),
      row('上次已存在', String(j.unchanged), '—',
        '已有自管库、这一轮什么都没动的分类数（幂等命中）'),
      row('结果未知', String(j.uncertain), '—',
        '建库请求超时、Jellyfin 侧是否建成不确定的条数（不计入新建）。下一轮同步会以列库结果收口'),
      row('被拒分类', j.failed.length === 0 ? '—' : el('span', { class: 'mono', text: j.failed.join('、') }), '—',
        'Jellyfin 拒绝建库的分类：多半是库路径在 Jellyfin 容器内不存在。修好路径后下一轮自动补上'),
      row('孤儿库', orphans, '—',
        '分类已消失的自管库。默认不删，删除走 POST /api/jellyfin/libraries {"prune":true}'),
      row('冲突库', conflicts, '—', '同名库不是本服务建的：不建、不删、不接管'));
    jellyfinBody.replaceChildren(...rows);
  }

  // fillJellyfin 把媒体库配置填进表单并记基线；api key 永远留空（只写不读）。
  function fillJellyfin(j) {
    jellyfinBaseline = {};
    for (const f of jellyfinFields) {
      jellyfinBaseline[f.key] = String(j[f.view]);
      document.getElementById(f.id).value = jellyfinBaseline[f.key];
    }
    setJellyfinKeyPlaceholder(j);
  }

  // setJellyfinKeyPlaceholder 只更新 key 输入框的占位符与清空值：值不回显。
  function setJellyfinKeyPlaceholder(j) {
    const input = document.getElementById('set-jellyfin-api-key');
    input.value = '';
    input.placeholder = j.api_key_set ? '已设置，留空不改' : '未设置（这一段不启用）';
  }

  // syncJellyfin 手动触发一轮媒体库同步：202 = 已派出去，409 = 上一轮还在跑，
  // 400 = 三项没配齐（响应体点名缺哪个键）。
  //
  // 202 不带结果，所以派出去之后立刻回读一次 /api/status，记下这一轮的 run_seq，
  // 之后靠轮询等它收尾（见 afterRender）：run_seq 够大且 running=false 才算「这一轮跑完了」。
  async function syncJellyfin() {
    jellyfinBtn.disabled = true;
    say('已请求同步 Jellyfin 媒体库 …', 'busy');
    try {
      await api('POST', '/api/jellyfin/libraries', {});
    } catch (err) {
      if (err.status !== 409) {
        fail(err);
        syncHint();
        return;
      }
      // 409 不是失败：单飞锁挡住了第二次派发，页面转为盯正在跑的那一轮。
      say('已有一轮媒体库同步在跑，没有重复派发（下面显示它的进度）', 'busy');
    }
    // 202 不带结果：回读一次 /api/status 认领这一轮的 run_seq，之后靠 poll 等它收尾。
    // 极短的一轮（0 变化）可能已经结束了，afterRender 会直接把结果报出来。
    try {
      const status = await api('GET', '/api/status');
      watchingSeq = status.jellyfin.run_seq;
      pollDeadline = 0;
      applyStatus(status);
      afterRender(status.jellyfin);
    } catch (err) {
      fail(err);
    } finally {
      syncHint();
    }
  }

  async function load() {
    const status = await api('GET', '/api/status');
    applyStatus(status);
    afterRender(status.jellyfin);
    syncHint();
  }

  // applyStatus 把一份 /api/status 画到页面上。轮询与 load 共用它：
  // 一次轮询就是一次 load，两者渲染路径只有一条。
  function applyStatus(status) {
    const t = status.totals;
    totalsBox.replaceChildren(
      stat('剧目', String(t.dramas)),
      stat('集', String(t.episodes)),
      stat('对外集', String(t.emby_items)),
      stat('最近同步', fmtTime(t.last_sync_at), true));
    renderConfig(status.config);
    renderJellyfin(status.jellyfin);
    sourcesBody.replaceChildren(...status.sources.map(sourceRow));
    adminWritable = status.admin_token_set === true;
    if (!prefilled) {
      prefilled = true;
      fillSettings(status.config);
      fillJellyfin(status.jellyfin);
    } else {
      // 凭据输入框每次回读都清空：值不回显，占位符按最新的「已设置/未设置」更新。
      setCredentialPlaceholders(status.config);
      setJellyfinKeyPlaceholder(status.jellyfin);
    }
  }

  // afterRender 决定「还要不要继续轮询」：等的那一轮收尾了 → 停下并给一句结果；
  // 还在跑 → 1 秒后再来一次；既没在等、也没在跑 → 停。页面刷新后接着轮询
  // （那时 watchingSeq 为 0，只显示进度、不替用户宣布别人的结果）。
  function afterRender(j) {
    if (watchingSeq !== 0 && j.run_seq >= watchingSeq && !j.running) {
      watchingSeq = 0;
      pollDeadline = 0;
      stopPoll();
      say(resultText(j), resultKind(j));
      return;
    }
    if (j.running) {
      poll();
      return;
    }
    stopPoll();
    pollDeadline = 0;
  }

  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    const payload = {};
    for (const [key, id] of Object.entries(fields)) {
      const value = document.getElementById(id).value.trim();
      if (value !== baseline[key]) payload[key] = value;
    }
    for (const [key, id] of Object.entries(credentials)) {
      const value = document.getElementById(id).value.trim();
      if (value !== '') payload[key] = value;
    }
    for (const f of jellyfinFields) {
      const value = document.getElementById(f.id).value.trim();
      if (value !== (jellyfinBaseline[f.key] || '')) payload[f.key] = value;
    }
    // key 只写不读：留空 = 不改（要清空就显式传空串，这条只走 API）。
    const jellyfinKey = document.getElementById('set-jellyfin-api-key').value.trim();
    if (jellyfinKey !== '') payload.jellyfin_api_key = jellyfinKey;
    if (Object.keys(payload).length === 0) {
      say('没有改动', 'ok');
      return;
    }
    saveBtn.disabled = true;
    say('正在写入配置 …', 'busy');
    try {
      const data = await api('PATCH', '/api/settings', { settings: payload });
      // 回显归一化后的值（例：strm 前缀的尾 `/` 已被去掉），并把基线更新成刚写进去的值。
      for (const [key, value] of Object.entries(data.settings)) {
        const id = fields[key];
        if (!id || value === null) continue;
        document.getElementById(id).value = String(value);
        baseline[key] = String(value);
      }
      for (const f of jellyfinFields) {
        const value = data.settings[f.key];
        if (value === undefined || value === null) continue;
        document.getElementById(f.id).value = String(value);
        jellyfinBaseline[f.key] = String(value);
      }
      for (const id of Object.values(credentials)) document.getElementById(id).value = '';
      document.getElementById('set-jellyfin-api-key').value = '';
      say('已写入：' + Object.keys(data.settings).join('、') + '（生效时机见「运行配置」）', 'ok');
      await load();
    } catch (err) {
      fail(err);
    } finally {
      syncHint();
    }
  });

  jellyfinBtn.addEventListener('click', syncJellyfin);

  // 顶栏令牌改动后立刻更新提示与按钮状态。
  const tokenInput = document.getElementById('admin-token');
  if (tokenInput) tokenInput.addEventListener('input', syncHint);

  load().catch(fail);
}

// ---- 分派 ----

initAdminToken();
const page = document.body.dataset.page;
if (page === 'dramas') initDramas();
else if (page === 'status') initStatus();
