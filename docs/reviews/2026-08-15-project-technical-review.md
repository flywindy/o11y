# o11y 專案技術審查報告(2026-08-15)

> 審查範圍:核心 SDK(root、`internal/`)、九個整合套件、`k8s/` 基礎設施、CI/CD 與工具鏈。
> 審查角度:資深後端工程師 / SRE / 架構師。
> 審查方法:全量程式碼閱讀 + `go build`/`go vet`/`go test`(全數通過)+ `go list -deps` 相依驗證 + manifest 交叉比對。

---

## 總評

這是一個**遠高於平均水準**的 observability SDK 專案:25 篇 ADR 完整記錄設計決策、以
`scripts/check_integrations.go` 把 ADR 0003/0008 政策做成 CI 閘門、無全域 OTel 狀態污染、
冪等的 Shutdown、初始化失敗路徑的資源清理、以及對 metric cardinality 的系統性防護,都是
少見的工程紀律。測試在 `-race` 下全綠,註解一貫解釋「為什麼」而非「做什麼」。

但在更廣泛採用(多團隊引用)之前,有**四個必須先處理的結構性/正確性問題**(P0),
以及一批一致性債與基礎設施缺陷。以下依優先級列出。

---

## P0 — 廣泛採用前必須修復

### A1.(架構)root 套件把四個資料庫 driver 連結進每個使用者的 binary

`o11y.go:24-32、250-256` 為了收集 `MetricViews(...)` 直接 import 了
`cassandra/`、`minio/`、`mongo/`、`redis/` 子套件。已用 `go list -deps` 驗證:
**任何只想要 tracing+logging 的服務,import root 套件後,binary 會被連結進
gocql、minio-go/v7、mongo-driver/v2(含 AWS auth stack)、go-redis/v9 全套。**

影響:
- 使用者繼承所有 driver 的 CVE 面(govulncheck 噪音)、binary 體積、`go.sum` 膨脹;
- MVS 版本衝突風險(使用者若釘住舊版 mongo-driver 會與 SDK 打架);
- 相依方向反轉:SDK core 不應依賴整合層。

各 `views.go` 本身只 import OTel(view 定義是純字串 scope 比對),但 Go 以套件為連結單位,
import 套件就會連結整包。**修法(擇一,由小到大):**
1. 把每個整合的 view 定義搬到 leaf 套件(如 `redis/views` 子套件或 `internal/views`,
   以 scope-name 字串常數比對,不 import driver),root 改 import leaf;
2. root 不再主動收集 views,改由整合套件在 wiring 時透過既有的 `ExtraViews` seam 註冊;
3. 長期:仿 otel-contrib 拆成 per-integration Go modules(`o11y/redis` 各自獨立
   `go.mod`),徹底隔離相依。單一 module 目前也把 `nats-io/nats-server/v2`
  (僅測試用的完整 NATS server)列為 direct dependency,拆 module 可一併解決。

### A2.(核心正確性)`Logger.WithGroup` 之後 `traceId`/`spanId` 被巢狀進群組,破壞 log-trace 關聯契約

`internal/log/handler.go:27-35, 49-51` — `OtelSlogHandler.Handle` 在 `WithGroup`
已委派給底層 JSON handler 之後才用 `r.AddAttrs` 注入 traceId,record-level attr 會被
qualify 進打開的群組。因此:

```go
sdk.Logger.WithGroup("req").ErrorContext(ctx, ...)
// stdout 輸出 {"req":{"traceId":...}} 而非頂層 traceId
```

`o11y.go:64-70` 承諾的頂層關聯欄位對任何 `WithGroup` 衍生 logger 靜默失效,
Loki/Fluentd 查詢與 trace 關聯直接斷掉;且 stdout 與 OTLP 兩路輸出(otelslog 從 ctx
取 trace context,維持 record-level)行為分歧。此路徑無測試。修法:handler 記錄群組
深度、或在 pre-group base handler 注入。

### A3.(resty)request-level retry condition 觸發重試時,attempt span 洩漏(永不 End、metric 少記)

`resty/hook.go:292-302` 的 `retryableResponse` 只檢查 `c.RetryConditions`,但 resty
v2.17.2 實際會合併 request-level 條件(`request.go:1071`)。使用者用
`req.AddRetryCondition(...)`(無 client-level 條件)時,`afterResponse` 不會結束
attempt-1 的 span,`h.retry` 又因 `err == nil` 提前返回 —— 每次重試漏掉一個 span
(永不匯出)與一筆 `http.client.request.duration` 樣本;retry-exhausted 標記同樣失效。
無對應測試。

### A4.(redis)套件自己發出的 pool metrics 沒有任何 view —— `db.client.connection.create_time` 用預設(毫秒級)bucket 匯出

`redis/views.go:21-63` 只設定 `db.client.operation.duration` 與舊版 redisotel 複數名
的 drop-views;`redis/metrics.go:32-79` 實際發出的單數名 instrument
(`db.client.connection.count/.idle.max/.idle.min/.max/.timeouts/.create_time`)無 view 匹配。
`create_time` 單位是秒,卻沿用 OTel 預設邊界 `[0,5,10,…10000]`,幾乎所有 dial 都落在第一個
bucket,直方圖對 pool sizing(該套件明文宣稱的用途,`redis/hook.go:88-95`)完全無用。
mongo 與 cassandra 在相同 instrument 上都有釘 bucket 與 allow-keys,redis 是唯一漏掉的。

---

## P1 — 高優先(正確性 / 一致性 / 基礎設施)

### 核心 SDK

| # | 位置 | 問題 |
|---|------|------|
| B1 | `internal/metrics/metrics.go:373-388` | OTLP push 路徑:`runtime.Start` 失敗時只 shutdown exporter,`MeterProvider`(含 PeriodicReader goroutine)洩漏,每個 interval 對已關閉的 exporter 報錯。Prometheus 路徑(`initPrometheus`)有正確處理,兩邊應對稱。 |
| B2 | `options.go:603` + 各 exporter 一律傳 `WithEndpointURL` | 標準 `OTEL_EXPORTER_OTLP_*ENDPOINT` 環境變數**永遠被靜默覆蓋**(預設 `http://localhost:4318` 無條件生效),與 SDK 尊重 `OTEL_TRACES_SAMPLER`、`OTEL_RESOURCE_ATTRIBUTES`、`OTEL_EXPORTER_OTLP_HEADERS` 的行為不一致。用標準 OTel 部署清單的服務會把遙測送去 localhost 而毫無警告。建議:caller 未設 `WithOTLPEndpoint` 時尊重 env var,或至少在文件與啟動 log 明示。 |
| B3 | `internal/profiling/profiling.go:85-93` | profiler `Stop()` 失敗時 `profilerStarted` 不重置,配合 `Shutdown` 的 `sync.Once`,整個 process 永久無法再啟動 profiling。建議無條件重置。 |
| B4 | `internal/log/handler.go:30-34, 96-105` | `Handle` 未 `Clone` 就 `AddAttrs`,違反 slog handler 撰寫指南。SDK 內部因 `MultiHandler` 有 clone 而安全,但 `sdk.Logger.Handler()` 是公開的,使用者自行 fan-out 時會跨 handler 汙染。 |
| B5 | `o11y.go:358-360, 388-390` | profiling endpoint 原文寫入 log;URL 內嵌 userinfo(`http://user:pass@host`)會把憑證洩進 log。建議 log 前 redact userinfo。 |

### 整合套件

| # | 位置 | 問題 |
|---|------|------|
| C1 | `http/server.go:17-19`、`gin/middleware.go:17-19` | 文件承諾「絕不回退到全域 OTel state」,但 otelhttp/otelgin 的 option 對 nil 是忽略,`NewServerHandler(next, nil, nil, nil)` 會**靜默使用全域 provider** —— 正是文件排除的行為。且九個套件對 nil provider 有三種姿態(回錯誤 / 靜默 no-op / 靜默用全域),應統一(建議:一律回錯誤,與 redis/mongo/cassandra/nats/es 對齊)。 |
| C2 | `cassandra/observer.go:104-108, 270-286` | `db.namespace` label 來自解析 statement 文字,**無 value cap**(`WithMaxUniqueCollections` 只管 `db.collection.name`)。keyspace-per-tenant 部署或 tokenizer 誤判時 cardinality 無界。 |
| C3 | `mongo/pool_metrics.go:146-160, 214-219, 323-339` | (a) fallback pool 名含單調遞增序號,client 重建(重連/配置重載)時每次產生新 label value,舊 series 以 sync UpDownCounter 永久留存 → 緩慢無界成長;(b) `cleanup()` 在 `Disconnect` 之前執行(兩個 defer 的自然順序)會吞掉歸零事件,`db.client.connection.count` 凍結在非零值。 |
| C4 | `minio/client.go:353-382` | `ListObjects` 消費者棄讀 channel 且 ctx 不可取消時,轉發 goroutine 永久阻塞 **且 wrapper 額外掛著一個未結束的 span**;ListObjects 完全無測試。 |
| C5 | 低嚴重度批次 | ES 對 3xx 標 span Error(與 SDK 其他 HTTP 套件 ≥400 不一致);redis `commandText` 先組完整字串(10MB SET → 10MB 暫時配置)再截斷;mongo pool 事件單一 mutex 且 `Add` 在臨界區內;resty 錯誤路徑丟失 `server.address`;resty 無 `OnPanic` hook(panic 時 span 不 End);minio metric 樣本缺 system attribute。 |

### K8s 基礎設施(kind 開發環境定位,但需修)

| # | 位置 | 問題 |
|---|------|------|
| D1 | `k8s/infrastructure/base/{cassandra,elasticsearch}.yaml` | **未被任何 kustomization 引用的重複 manifest,且已漂移**:頂層 elasticsearch.yaml:32 是 `runAsGroup: 0`(root group),components 版本是 `1000`。直接刪除兩個頂層檔案。 |
| D2 | `components/nats/nats.yaml:10` | JetStream 已啟用但 StatefulSet **完全沒有儲存 volume**(連 emptyDir 都沒有),stream/KV 寫在 overlay fs,容器重啟即消失。至少加 volumeClaimTemplate 或明示 emptyDir。 |
| D3 | `monitor/otel-collector.yaml:25-28, 92-102` | 全部遙測的單一入口沒有 `memory_limiter` processor、沒有 resource limits、沒有 probes(collector 有 `health_check` extension 可用)。遙測突發時被 OOMKill、in-flight 資料全丟。 |
| D4 | `monitor/otel-collector.yaml:35-50, 67-70` | 使用**已被 collector-contrib 移除的 deprecated `loki` exporter**,image 一升版就起不來。改用 `otlphttp` exporter 打 Loki 原生 OTLP endpoint(`/otlp`),並連動調整 Grafana derived-field regex。 |
| D5 | monitor 全部 + mongodb + nats | 整個 monitor stack(prometheus/loki/tempo/grafana/alloy)無 probes、無 resources、無 securityContext、全部 emptyDir(Grafana 連 volume 都沒有);與 minio/cassandra/es/pyroscope 的模範寫法(non-root、drop ALL、seccomp、PVC、明確的 production-override 註解)形成強烈反差 —— **最容易被複製到 production 的那一半反而最沒防護**。mongodb 無認證且以 root 執行,亦無 dev-only 註解。 |
| D6 | `monitor/grafana.yaml:117-118` vs `monitor/tempo.yaml` | Grafana service map 指向 Prometheus,但 Tempo 沒開 `metrics_generator` → node graph 永遠空白的死功能。二擇一:補 metrics_generator 或移除設定。 |
| D7 | `kind-config.yaml:5-11` | 未設 `listenAddress`,host 的 4318(無認證 OTLP)、4223(`no_tls` WebSocket NATS)、4040(Pyroscope ingest)綁 `0.0.0.0`,共享網路上任何人可注入。加 `listenAddress: "127.0.0.1"`。 |
| D8 | Loki / Prometheus | 無 retention 設定(Loki 預設不啟用 retention → 無界成長;Prometheus 無 size cap),長壽 kind node 會磁碟壓力驅逐其他 pod。 |

### CI/CD 與工具鏈

| # | 問題 |
|---|------|
| E1 | CI 註解多處聲稱「由 Dependabot/Renovate 管理升級」,但 repo **沒有 `.github/dependabot.yml`**:SHA-pinned actions、golangci-lint v2.11.4、govulncheck v1.3.0、`GOVULNCHECK_GOTOOLCHAIN` pin 會靜默腐化,vuln job 的價值隨 toolchain 老化衰減。 |
| E2 | `k8s/` 完全沒有 CI 驗證(無 `kustomize build`、kubeconform、yamllint)—— D1 的漂移與 D4 的 deprecated exporter 正是這類 job 能攔下的。 |
| E3 | `CLAUDE.md`/`GEMINI.md` symlink 指向 `C:/Users/SheepRocket/Projects/o11y/AGENTS.md`(Windows 絕對路徑),在作者機器以外全部壞掉。改成相對 symlink `AGENTS.md`。 |
| E4 | 其他:coverage 有上傳但無門檻;無 release/tag 自動化(CHANGELOG 手維護);`internal/trace` 無測試檔;action SHA pin 缺 `# vX.Y.Z` 註解;Makefile `examples` 缺 CI 版的 no-dir guard;`docs/guide.md:293` 連到已搬移的 `base/prometheus.yaml`;`.golangci.yml` 可考慮加 `gosec`。 |

---

## P2 — 觀察與建議(非缺陷)

1. **相依重量**:上游 `akira-core/instrumentation-go/otel-nats` 拖進 open-feature、
   go-feature-flag、antlr、quic-go 等大量與 NATS instrumentation 無關的間接相依,
   建議向上游反映或評估 fork 瘦身。
2. **cardinality 內部預算**:`cardinalityLimitBudget(1000,200)` = 1000×16×64 ≈ 102 萬
   attribute sets/stream,作為記憶體防線偏名義性(失控 key 可先吃數百 MB),16×64
   乘數值得回頭檢視。
3. **`/metrics` server**:`server.Serve` 錯誤被完全吞掉(bind 後失敗會靜默死掉);
   `:0` 綁定後無法取得實際 port(測試用 TOCTOU workaround,平行下可能 flake)。
4. **`o11ytest.CanceledRequestContext`** 回傳的是活的 context,命名暗示已取消 —— 公開
   helper 的命名地雷。
5. **API 演進**:`resty.Wrap` 不回傳 error 而 `redis.Wrap` 回傳 `(client, error)`,
   未來 resty 需要錯誤路徑時是 breaking change;pre-1.0 現在統一最便宜。

---

## 值得保留與推廣的優點

- **ADR 紀律 + 政策即程式碼**:ADR 0003(無全域狀態)/0008(sourcing 三層模型)由
  `check_integrations.go` 在 CI 強制執行,semconv 基線(v1.39.0)與升級策略(ADR 0006)
  明確,上游升版有 span-name 基線測試對照(`docs/upstream-otel-nats.md`)。
- **初始化與關閉語意**:Init 失敗路徑逐層回收、Shutdown 冪等且 error join、
  profiling 的 warn-and-continue 與 trace-to-profile wrapper 的延遲安裝(避免懸空
  profile id)都設計得很細。
- **cardinality 工程**:export-boundary cap(route/collection)+ 共享預算 + scope 限定
  view + Prometheus label 正規化碰撞偵測,是多數內部 SDK 沒有的深度。
- **redis/resty 的 weak-pointer + `runtime.AddCleanup` 去重機制**、nats 的
  `wrapMessageBatch` goroutine 生命週期處理,經查證皆正確且無洩漏。
- **CI 基本盤**:SHA-pinned actions、race+coverage、govulncheck 用刻意打過補丁的
  toolchain(理由寫在註解)、least-privilege permissions、timeouts、concurrency 取消。
- 四個 datastore component manifest(minio/cassandra/es/pyroscope)是模範等級的
  dev manifest,可作為 monitor stack 補課的樣板。

---

## 建議執行順序

1. **本週**:刪除 D1 漂移重複檔;修 A2(traceId WithGroup)、A3(resty span 洩漏)、
   A4(redis views)—— 三者都是局部、便宜的修復;補 E3 symlink、E1 dependabot.yml。
2. **短期(1-2 sprint)**:A1 破除 root→integration import(先做 leaf-package 方案,
   per-module 拆分另開 ADR);B1/B3/B4;C1 統一 nil-provider 姿態(pre-1.0 是最後窗口);
   D2/D3/D4;E2 manifest CI。
3. **中期**:B2(OTLP env var 精確語意,建議寫成 ADR);C2/C3/C4;D5-D8 monitor stack
   補課(probes/resources/securityContext/retention + production overlay 註解);
   release 自動化與 coverage 門檻。
