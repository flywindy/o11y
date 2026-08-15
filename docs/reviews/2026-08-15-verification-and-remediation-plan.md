# 查證結果與修復計畫(2026-08-15)

本文件是對 [`2026-08-15-project-technical-review.md`](./2026-08-15-project-technical-review.md)
的**逐項查證**,以及據此排定的 PR 計畫。

查證方法(不採信原報告的斷言,一律重新取證):

- **實測重現** — 對 A2 / A3 / A4 / B2 / B4 寫可執行測試,觀察實際輸出;
- **上游原始碼比對** — 直接讀 `GOMODCACHE` 內 resty v2.17.2、otelhttp/otelgin v0.68.0、
  otlpconfig v1.44.0、pyroscope-go v1.3.0、sdk/metric v1.44.0 的實際程式碼;
- **原型驗證** — 在 repo 副本上實作 A1 的提議修法,確認可編譯並量測收益;
- **上游事實查核** — 對 Loki exporter 的棄用/移除狀態查證官方發佈紀錄。

所有查證產物均在 repo 外的 scratchpad,**repo 保持乾淨**(`git status` 為空)。

---

## 一、查證結果總表

| 編號 | 指控 | 判定 | 取證方式 |
|---|---|---|---|
| A1 | root 套件連結四個 DB driver | **確認** | `go list -deps` + 原型改造實測 |
| A2 | `WithGroup` 後 traceId 巢狀 | **確認** | 實測輸出重現 |
| A3 | resty request-level retry span 洩漏 | **確認** | 實測重現 + 上游原始碼 |
| A4 | redis pool metrics 無 view | **確認** | 實測 view 匹配 + 對照組 |
| B1 | OTLP metrics MeterProvider 洩漏 | **確認**(但近乎不可達) | 程式碼 + sdk/metric 原始碼 |
| B2 | `OTEL_EXPORTER_OTLP_*ENDPOINT` 被忽略 | **確認** | 實測(httptest 對照) |
| B3 | profiling `Stop` 失敗永久毒化 | **部分確認**(邏輯成立,現版本不可達) | pyroscope-go 原始碼 |
| B4 | slog `Handle` 未 `Clone` | **確認**(症狀為 `!BUG` 哨兵) | 實測掃描 |
| B5 | profiling endpoint 憑證洩漏 | **確認** | 程式碼 + net/http 原始碼 |
| C1 | nil provider 姿態不一致 | **確認** | otelhttp/otelgin 原始碼 + 九套件普查 |
| C2 | cassandra `db.namespace` 無 cap | **確認** | 程式碼 + parser 實測 |
| C3 | mongo pool metrics 兩個問題 | **確認**(嚴重度下修) | 程式碼 + aggregate 原始碼 |
| C4 | minio ListObjects 洩漏「且完全無測試」 | **部分不成立** | 見下 |
| D1 | 重複 manifest 漂移至 `runAsGroup: 0` | **確認** | `diff` + kustomization 反查 |
| D2 | NATS JetStream 無儲存 | **確認** | manifest 檢視 |
| D3 | Collector 無 memory_limiter/limits/probes | **確認** | manifest 檢視 |
| D4 | Loki exporter「已移除、升版即掛」 | **不成立** | 上游發佈紀錄 |
| E1/E3/E4 | 無 dependabot、symlink 壞、文件死連結 | **確認** | 檔案系統檢視 |

### 被推翻或下修的指控(誠實記錄)

**D4 —— 不成立。** 原報告稱 `loki` exporter 已在 0.121.0 前後被 collector-contrib 移除、
「升版即掛」。查證:該 exporter 自 2024-07-09 標記**棄用**,但**持續發佈至 v0.130.0**
(2025-07),遠超過專案釘的 0.121.0。**目前的 collector 沒有壞,升版也不會立刻壞。**
正確定位是「應排程遷移的技術債」,不是 P1 事故。

**C4 —— 部分不成立。** 阻塞洩漏屬實,但:(a) `minio/client.go:350-352` 已明文記載這是
繼承自 minio-go 的契約,非未知缺陷;(b)「ListObjects 完全無測試」**錯誤** ——
`client_test.go:436-443` 有測試,只是僅涵蓋完整排空的 happy path。降為 P2。

**另一項我自己提出後推翻的疑慮:** 我一度懷疑專案只有 CHANGELOG 沒有 git tag,
查證遠端後確認 **v0.1.0 ~ v0.11.0 共 14 個 tag 都存在**,消費者確實用得到語意化版本。此疑慮撤回。

### 查證過程中新發現(原報告未提)

| 新發現 | 位置 | 說明 |
|---|---|---|
| **N1** | `otlpconfig/options.go:340-342` | `WithOTLPHeaders` 對 env 是**取代而非合併** —— 設了選項就會靜默丟棄 `OTEL_EXPORTER_OTLP_HEADERS`。與 B2 同一組語意問題,應一併處理。 |
| **N2** | `otelgin/config.go:135-139` | otelgin 的 `WithMeterProvider` **沒有 nil 防護**(其他選項有),目前僅靠 `gin.go:47-49` 事後補檢查而倖免。若上游調整順序即成 nil dereference。 |
| **N3** | `mongo/client.go:85-95` | `Connect` facade 在成功路徑上**完全丟棄** cleanup,僅在建構失敗分支呼叫 —— 這反而讓 C3 的凍結問題對多數使用者不可達,但也代表 tracker 永不停用。 |

---

## 二、關鍵查證細節

### A1 — 相依耦合(已量化 + 已驗證修法可行)

在 repo 副本上移除 `o11y.go` 的四個整合 import、將 `ExtraViews` 改為 `nil`,
**編譯完全通過**,量測結果:

| 指標 | 修前 | 修後 | 差異 |
|---|---|---|---|
| 只用 tracing 的 binary | **27.7 MB** | **23.8 MB** | **−3.9 MB(−14%)** |
| 連結進的 driver 套件數 | **77** | **0** | gocql / minio-go / mongo-driver / go-redis 全數消失 |

且四個 `views.go` **本身都不 import driver**(唯一例外是 mongo 為了
`otelmongo.ScopeName` 這個**字串常數**),證明耦合純粹是 Go 套件層級的,拆解無技術障礙。

### A2 — traceId 巢狀(實測輸出)

```
無 group : {"msg":"...","traceId":"0102...","spanId":"0102..."}
WithGroup: {"msg":"...","req":{"traceId":"0102...","spanId":"0102..."}}   ← 掉進群組
WithAttrs: {"msg":"...","k":"v","traceId":"0102...","spanId":"0102..."}   ← 不受影響
```

### A3 — resty span 洩漏(實測重現)

```
client-level 條件    attempts=2  endedSpans=2   ← 正常
request-level 條件   attempts=2  endedSpans=1   ← 洩漏
```

精確化:洩漏量是 **N−1**(最後一次 attempt 一定會被 `OnSuccess`/`OnError` 收掉),
且**必須 `SetRetryCount > 0`** 才會觸發。根因是 resty 的
`r.retryConditions` 是**未匯出欄位**,o11y 結構上讀不到,所以不該自行重算重試判斷,
而應改為消費 resty 自己的 retry hook。

### A4 — redis view 缺漏(實測 + 對照組)

```
db.client.operation.duration        MATCHED   → ExplicitBucketHistogram
db.client.connection.create_time    NO MATCH  ← 單位 "s",卻套用 OTel 預設 [0,5,...,10000] 毫秒級邊界
db.client.connection.count/.idle.max/.idle.min/.max/.timeouts   NO MATCH
```

決定性證據:**mongo(`views.go:70`)與 cassandra(`views.go:53`)都有釘同一個
`create_time` instrument**,只有 redis 漏掉 —— 確定是疏漏,不是設計選擇。

### B2 — OTLP 環境變數(實測對照)

| 設定的環境變數 | 實際結果 |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | **被忽略**(遙測仍送 127.0.0.1:4318) |
| `OTEL_EXPORTER_OTLP_TRACES/LOGS_ENDPOINT` | **被忽略** |
| `OTEL_EXPORTER_OTLP_HEADERS` | 生效(但見 N1) |
| `OTEL_RESOURCE_ATTRIBUTES` | 生效 |
| `OTEL_TRACES_SAMPLER` | 生效 |

根因:`options.go:603` 無條件填入字面預設值,exporter 端 env 先套用、programmatic option
後覆蓋(`otlpconfig/options.go:92-95`),所以選項永遠贏。

### B3 / C3 / C4 的嚴重度校正

- **B3**:邏輯確實會永久毒化,但 pyroscope-go v1.3.0 的 `Stop()` 是
  `return nil` 硬寫死(`api.go:102-107`),**現版本不可達**,只有測試 seam 或未來上游改動才會踩到。
- **C3**:兩個問題都成立,但 `Connect` facade 丟棄 cleanup(N3),風險僅限**直接呼叫
  `Instrument` 且提早執行 cleanup** 的使用者。
- **C4**:見上方「被推翻」段。

---

## 三、修復計畫:12 個 PR,分四波

排序原則:**先修「使用者已經在承受、且修復本身低風險」的正確性 bug**,
再處理需要 ADR 與遷移期的結構性/語意變更,最後是不影響 AP 的基礎設施與工具。

> 標註說明:**AP 影響**指對「引用 o11y SDK 的應用程式」的影響。
> 🟢 無感 / 🟡 需注意(儀表板或設定) / 🔴 需協調(行為變更)

### 第一波:正確性 bug(可立即進行,彼此獨立可並行)

#### PR#1 — `fix(log)`: traceId 恆為頂層欄位 + 遵守 slog Clone 契約(A2 + B4)

- **改法**:`OtelSlogHandler` 改為記錄 `WithGroup` 深度;有群組時不用 `r.AddAttrs`,
  改在 pre-group 的 base handler 注入(或改寫 handler 鏈,讓 trace 注入永遠發生在群組開啟之前)。
  同時在 `OtelSlogHandler.Handle` 與 `BaggageHandler.Handle` 的一般分支加 `r = r.Clone()`。
- **測試**:新增 `WithGroup` / 巢狀 `WithGroup` / `WithGroup+WithAttrs` 的頂層欄位斷言;
  外部 fan-out 不再出現 `!BUG` 哨兵的回歸測試。
- **修前 AP**:任何用 `sdk.Logger.WithGroup(...)` 的服務,其 stdout log 的 traceId 掉進群組,
  Loki 的 `| json | traceId=...` 查詢與 Grafana log→trace 關聯**靜默失效**;
  外部若自行 fan-out SDK handler,log 會出現 `!BUG` 字樣。
- **修後 AP**:🟡 traceId 回到頂層。**若已有團隊將錯就錯、依 `req.traceId` 建了查詢或
  derived field,需同步更新**。CHANGELOG 需列為行為修正並附遷移說明。
- **風險**:低。行為回到文件承諾的樣子。

#### PR#2 — `fix(redis)`: 補上 pool metrics 的 views(A4)

- **改法**:在 `redis/views.go` 比照 mongo/cassandra,為 `db.client.connection.create_time`
  釘 `histogramBuckets`,並為其餘五個 pool instrument 加 allow-keys filter。
- **測試**:比照本次查證寫的 view 匹配測試,斷言六個 instrument 全部被覆蓋(防未來再漏)。
- **修前 AP**:`db_client_connection_create_time_bucket` 幾乎全部落在第一個 bucket,
  拿來估連線建立延遲會得到**錯誤結論**;pool 標籤無 allow-list 保護。
- **修後 AP**:🟡 直方圖變得可用。**bucket 邊界改變** —— 若有人已對這個(壞掉的)指標建了
  PromQL,查詢結果會變。因原指標本就無意義,實務風險低,但仍須寫進 CHANGELOG。

#### PR#3 — `fix(resty)`: 改為消費 resty 的 retry hook,修正 span 洩漏(A3)

- **改法**:不再用 `retryableResponse` 自行重算(結構上讀不到 request-level 條件),
  改為在 `h.retry` 移除 `err == nil` 的提前返回,由 resty 的重試決策直接驅動 span 結束;
  並把 `retryable` 旗標穿進 `finishResponse`,讓 `resty.retry.exhausted` 屬性在
  request-level 條件下也正確。
- **測試**:新增 `req.AddRetryCondition` 路徑的 span 數與 duration 樣本數斷言;
  補 `OnPanic` hook 與其測試。
- **修前 AP**:用 `req.AddRetryCondition` 的服務,每次重試**遺失 N−1 個 span 與
  N−1 筆 `http.client.request.duration`** —— 下游 client 錯誤率與 P99 被系統性低估。
- **修後 AP**:🟡 span 與樣本數**會上升**(這是修正,不是退化)。
  依賴 `http.client.request.duration` 計算 QPS/錯誤率的儀表板會看到階梯式上升,
  需事先公告,避免被誤判為流量異常或告警誤觸。

#### PR#4 — `fix`: 三個小修(B1 + B3 + B5)

- **改法**:(a) `initOTLP` 失敗路徑補 `provider.Shutdown`,與 Prometheus 路徑對稱;
  (b) profiling 的 `profilerStarted` 改為無條件重置(旗標語意是「是否佔用 pprof 槽」,
  而非「關閉是否乾淨」);(c) 新增 `redactURL` helper,對 log 中的 endpoint 抹除 userinfo。
- **修前 AP**:(a) 近乎不可達;(b) 現版本不可達;(c) 用
  `http://user:pass@pyroscope:4040` 形式帶認證者,憑證會以明文寫進 stdout **與 OTLP log 管線**
  (即離開 process)。
- **修後 AP**:🟢 無感。(c) 對已誤用 userinfo 的服務是純安全改善。

### 第二波:結構性變更(需 ADR,建議排在同一個 minor 版本)

#### PR#5 — `refactor`: 破除 root → integration 的相依邊(A1)+ ADR

- **改法**:將四份 view 定義下沉到不 import driver 的 leaf 套件
  (如 `internal/views/`,scope name 以字串常數表示);root 只 import leaf。
  **各整合套件保留 `MetricViews` 作為薄轉出**(`func MetricViews(b []float64) []sdkmetric.View { return views.Redis(b) }`),
  公開 API 完全不變。mongo 的 `otelmongo.ScopeName` 改為硬編碼常數,
  並在 mongo 套件(已 import otelmongo)加一致性斷言測試防漂移。
- **為何非破壞**:`MetricViews` 是 ADR 0013/0014/0018/0019 明文記載的公開 API
  (供自建 MeterProvider 的服務註冊),必須保留 —— 轉出方案完整保住它。
- **修前 AP**:只要 import `github.com/flywindy/o11y`,即使只用 tracing,
  也會被連結進 gocql + minio-go + mongo-driver + go-redis(77 個套件),
  繼承其 CVE 面與版本約束,binary 多 3.9 MB。
- **修後 AP**:🟢 幾乎無感,且是純收益:binary −14%,`go mod tidy` 後四個 driver
  從 `go.sum` 消失,govulncheck 噪音下降。
  **唯一風險**:若有服務(不良實踐地)依賴這條傳遞相依而未在自己的 `go.mod` 顯式 require,
  升級後會編譯失敗 —— 修法是自行 `go get` 該 driver。需在 CHANGELOG 明示。

#### PR#6 — `fix(http,gin)`: 統一 nil provider 政策(C1)+ ADR 0003 修訂

- **改法**:九個套件目前有**三種**姿態(回錯誤 / 靜默 noop / 靜默用全域)。
  `http`/`gin` 的簽章不回傳 error,改成回錯誤是破壞性變更;因此建議走 noop 路線:
  在 facade 內對 nil 替換為 noop provider 與空 composite propagator,
  **真正兌現「絕不回退全域」的文件承諾**。同時在 ADR 0003 把政策寫死成一條規則,
  並補上 otelgin `WithMeterProvider` 未 nil 防護(N2)的 defensive 處理。
- **修前 AP**:傳 nil 的服務會**靜默使用全域 provider**,與文件及 ADR 0003 矛盾;
  這是 http/gin 兩個最常用套件唯一違反自身契約之處。
- **修後 AP**:🔴 **需協調**。若有服務同時 (i) 傳 nil 且 (ii) 呼叫過
  `otel.SetTracerProvider(...)`,今天靠全域仍有 telemetry,**修後會變成 noop、遙測消失**。
  這類服務其實是誤用,但「遙測突然不見」很難除錯。
  緩解:PR 內附掃描指引(grep `NewServerHandler(.*nil`),CHANGELOG 列為 Breaking,
  並建議這些服務改傳 `sdk.TracerProvider()`。

### 第三波:語意變更(影響最大,需公告與遷移期)

#### PR#7 — `feat`: 尊重標準 OTLP 環境變數(B2 + N1)+ ADR

- **改法**:`otlpEndpoint` 預設改為空哨兵,**僅在呼叫端明確設定 `WithOTLPEndpoint` 時
  才傳 `WithEndpointURL`**,把標準 env 優先鏈完整交還給 OTel SDK。
  headers 同理(改為合併或僅在設定時傳,修正 N1 的靜默丟棄)。
- **修前 AP**:k8s manifest 中設了 `OTEL_EXPORTER_OTLP_ENDPOINT` 的服務,
  遙測仍固執地送往 `localhost:4318` —— 用標準 OTel 部署範本的團隊會遇到「遙測憑空消失」。
- **修後 AP**:🔴 **本計畫中風險最高的一項,必須先盤點再上線**。
  若某服務的環境中**已經**設有 `OTEL_EXPORTER_OTLP_ENDPOINT`(可能是叢集層級注入、
  或其他 SDK 的遺留),修後遙測會**突然改道**到那個位址。
  建議流程:(1) 先發一版**只在偵測到 env 與 SDK 預設衝突時印 WARN** 的觀測版本;
  (2) 收集一至兩週,確認哪些服務會受影響;(3) 再切換實際優先序。
  絕不可與其他變更混在同一個 release 出。

#### PR#8 — `fix(cassandra)`: 為 `db.namespace` 加上 cardinality cap(C2)

- **改法**:在 `otlpCapRules`/`prometheusCapRules` 增加第三組規則,鍵為
  `semconv.DBNamespaceKey` / `db_namespace`,限定 cassandra scope,
  與 collection 共用或獨立預算(建議新增 `WithMaxUniqueNamespaces`);
  `cardinalityLimitBudget` 一併納入新維度。
- **修前 AP**:keyspace-per-tenant 部署,或 tokenizer 誤讀
  (實測可產生 `ns="'user-supplied"`、`ns="192.168.1"` 這類值)時,
  `db.namespace` 在兩個 instrument 上**無界成長**,可能打爆 Prometheus。
- **修後 AP**:🟡 超過上限的值收斂為 `"other"`。正常 schema(數十個 keyspace)不受影響;
  出現 `other` 反而是應調查的訊號。

#### PR#9 — `fix(mongo)`: pool metrics 的凍結與無界標籤(C3)

- **改法**:(a) `cleanup()` 改為**自己先發出歸零**(持鎖走訪 `t.pools` 呼叫 `closePool`)
  再設 `disabled`,讓執行順序不再影響正確性;(b) fallback pool 名改為由 host 集合
  推導的**確定性名稱**(或在 metrics 開啟時要求 `WithPoolName`),
  讓 client 重建重用既有 series 而非每次新增。
- **修前 AP**:直接用 `Instrument` 且提早呼叫 cleanup 者,
  `db.client.connection.count` 會**永久凍結在非零值**(誤導容量判斷);
  會重建 client 的服務則緩慢累積永不消失的 series。
- **修後 AP**:🟡 (b) 會**改變 `db.client.connection.pool.name` 的標籤值**,
  依該標籤分組的儀表板需更新。建議與 PR#8 同一版發布並集中公告。

### 第四波:基礎設施與工具(不影響 AP,可與前三波並行)

#### PR#10 — `fix(k8s)`: 安全與資料正確性(D1, D2, D3, D5, D7, D8)

刪除兩個漂移的重複 manifest(含 `runAsGroup: 0`);為 NATS JetStream 補
`volumeClaimTemplates`;Collector 加 `memory_limiter` + resources + `health_check` probes;
monitor stack 比照 minio/cassandra 補 probes/resources/securityContext 並加上
「production 必須覆寫」註解;`kind-config.yaml` 加 `listenAddress: "127.0.0.1"`;
Loki/Prometheus 補 retention。**AP 影響:🟢 無**(僅本地 kind 開發叢集)。

#### PR#11 — `chore(k8s)`: Collector 遷移 loki → otlphttp(D4)+ Grafana service map(D6)

改用 `otlphttp` 打 Loki 原生 OTLP endpoint,連動調整 Grafana derived-field regex 與
label 對應;service map 二擇一(補 Tempo `metrics_generator` 或移除該設定,不留死功能)。
**注意:此為排程技術債,非緊急** —— 現行 0.121.0 的 loki exporter 仍可運作。
**AP 影響:🟢 無**,但 Loki 的 label 對應會變,查詢語法需同步更新。

#### PR#12 — `chore`: CI 與工具鏈(E1–E4)

新增 `.github/dependabot.yml`(讓既有的 SHA pin 與版本 pin 真的有人管);
新增 manifest 驗證 job(`kustomize build` + kubeconform —— 這正是能攔下 D1/D4 的機制);
修 `CLAUDE.md`/`GEMINI.md` 的 Windows 絕對路徑 symlink 為相對路徑;
修 `docs/guide.md:293` 死連結;補 `internal/trace` 測試;
評估加入 `gosec`。**AP 影響:🟢 無**。

---

## 四、建議的發布節奏

| 版本 | 內容 | 對 AP 的要求 |
|---|---|---|
| **v0.12.0** | PR#1–#4(第一波)+ PR#10, #12 | 升版即可。需留意 PR#1/#2/#3 的儀表板調整 |
| **v0.13.0** | PR#5, #6(第二波)+ PR#11 | PR#6 需先掃描 nil provider 誤用 |
| **v0.14.0** | PR#8, #9 | 標籤值變更,集中公告 |
| **v0.15.0** | PR#7(單獨發布) | **必須先跑觀測版本盤點環境變數** |

分波的理由:**把「遙測會改變」的變更彼此隔開**。若 PR#3(span 數上升)、
PR#7(遙測改道)、PR#9(標籤改名)擠在同一版,任何一個服務出現異常時,
值班的人無法判斷是哪一項造成的 —— 這對一個 observability SDK 是特別諷刺的失敗模式。

## 五、額外建議:替 AP 準備遷移工具

由於本計畫含三項會改變遙測形狀的變更,建議在第一波就附帶:

1. **一份升級檢查清單**(放 `docs/upgrade/`),列出每版需檢查的 PromQL/LogQL 樣式;
2. **一個 grep 腳本**,協助服務自查是否踩到 `NewServerHandler(..., nil, ...)`、
   `req.AddRetryCondition`、`WithGroup` + traceId 查詢、`OTEL_EXPORTER_OTLP_ENDPOINT` 這四種情境;
3. CHANGELOG 既有的「Breaking Changes (Migration Guide)」區塊品質已經很好,延續即可。
