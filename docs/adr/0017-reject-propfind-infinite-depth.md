# ADR-0017：`PROPFIND` 的 `Depth: infinity` 一律回 403

状态：**已接受**（2026-11-04，MKACG-678）

## 背景

`/webdav/` 是只读 WebDAV 挂载点，目录列举由 `x/net/webdav` 实现。它的 `handlePropfind`
在缺 `Depth` 头时默认 `infiniteDepth`（RFC 4918 §9.1 规定的语义），并真的递归整棵子树。

线上实测（NAS 上的实例，2026-11-04）：

| 请求 | 结果 |
|---|---|
| `PROPFIND /webdav/` `Depth: 1` | 207，17–27 KB，毫秒级 |
| `PROPFIND /webdav/` `Depth: infinity` | **超过 120 秒仍未返回** |
| `PROPFIND /webdav/东方仙侠/` `Depth: infinity` | 207，513 KB，4.51 s |
| `PROPFIND /webdav/<分类>/<剧>/` `Depth: infinity` | 207，4235 B，0.06 s |

成本与子树规模线性：根下是全部分类，单个「剧情」分类就有 8,329 部剧；21.6 万剧的整树
递归在 MKACG-651 的冷启动实测里是 369 MB / 116 s / RSS +501 MB。

会主动发这个形状的客户端（Windows 资源管理器、部分 Android 文件管理器与挂载工具）等不到
结果，只会看到连接超时，于是判定挂载失败——它们不知道这个服务只是慢。

ADR-0013 当时明确「本服务刻意不拦截它」：拦截要改 `Depth` 支持，超出那一次任务的契约。
现在线上已经证明它不是理论风险，而是会直接造成挂载失败，因此单独定这条决策。

## 决策

在 `internal/webdav` 的 `Handler.ServeHTTP` 里，**认证通过之后、`x/net/webdav` 之前**拦截：

1. 只拦 `PROPFIND`。`COPY` / `MOVE` / `LOCK` 也接受 `infinity`，但它们是写意图，语义与
   目录列举无关（且本服务只读），不改。
2. 「显式 `Depth: infinity`」与「省略 `Depth` 头」一起拦：两者在 `x/net/webdav` 里是同一个
   `infiniteDepth`，只拦前者会给 `curl -X PROPFIND <url>` 留下同一条挂死路径。
3. 回 `403 Forbidden` + `Content-Type: application/xml; charset=utf-8`，主体是
   RFC 4918 §9.1 的错误体：

   ```xml
   <?xml version="1.0" encoding="utf-8"?>
   <D:error xmlns:D="DAV:"><D:propfind-finite-depth/></D:error>
   ```

   客户端据此知道服务端只支持有限深度，按 RFC 改回递归 `Depth: 1`。
4. 数值深度 ≥2 不在拦截范围，仍由协议库回 `400`（既有语义，不改）。
5. 拦截发生在响应缓存（ADR-0015）之前：请求不查树、不渲染、不进缓存。

顺带：`OPTIONS` 的 `Allow` 由协议层按集合/文件两种形状生成，集合分支漏了本服务自
MKACG-671 起真的支持的 `GET, HEAD`（集合 GET/HEAD 出目录页）。`serveOptions` 在协议层返回
后补上这两个方法（`handleOptions` 只写响应头、不写状态码，响应尚未提交）。

## 被否决的做法

- **截断：递归到一定层数/节点数就停止或回部分结果**。违反 `PROPFIND` 的响应契约——客户端
  会把不完整的 multistatus 当成真实内容，比 403 更难排查。
- **把 `infinity` 当 `Depth: 1` 处理**。语义撒谎：客户端以为拿到了整棵树。RFC 给的正确应答
  就是 403 + `propfind-finite-depth`。
- **只拦根目录**。分类层 4.51 s、剧层毫秒级看着「还行」，但成本随规模线性，21.6 万剧的
  分类目录同样是上百秒；策略要一致，否则只是把雷留到下一层。
- **加一个「是否支持 infinity」的 settings 开关**。硬约束 5 管的是可调参数，而这里没有合理
  的第二个取值：放开 `infinity` 换来的是挂死，不是能力。
- **恢复目录分片层来压低单棵子树规模**。MKACG-651 已在 `jellyfin.db` 上实测推翻（Jellyfin
  会把分片目录当 Season），与本决策无关。

## 后果

- 客户端发 `infinity` 或省略 `Depth` 会立刻拿到 `403` 与 `propfind-finite-depth`，需要按 RFC
  改回 `Depth: 1`；rclone / Emby / Jellyfin 本来就逐层 `Depth: 1`，不受影响。
- `docs/DIRECTORY-CONTRACT.md`「单目录子项上限」与 `docs/DEV-STANDARDS.md` 的 `Depth` 节保留
  拦截前的实测数据（那是不能放开它的依据），但「本服务不拦截」的表述已随本 ADR 失效；
  ADR-0013 的同名表述以本 ADR 为准。
- 一个此前会跑 120 秒以上的请求变成 O(1) 的 403；没有新增缓存、没有改树形（硬约束 10）。
