package decision

import (
	"encoding/json"
	"fmt"
	"log"
	"nofx/market"
	"nofx/mcp"
	"nofx/pool"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PositionInfo 持仓信息
type PositionInfo struct {
	Symbol           string  `json:"symbol"`
	Side             string  `json:"side"` // "long" or "short"
	EntryPrice       float64 `json:"entry_price"`
	MarkPrice        float64 `json:"mark_price"`
	Quantity         float64 `json:"quantity"`
	Leverage         int     `json:"leverage"`
	UnrealizedPnL    float64 `json:"unrealized_pnl"`
	UnrealizedPnLPct float64 `json:"unrealized_pnl_pct"`
	LiquidationPrice float64 `json:"liquidation_price"`
	MarginUsed       float64 `json:"margin_used"`
	UpdateTime       int64   `json:"update_time"` // 持仓更新时间戳（毫秒）
}

// AccountInfo 账户信息
type AccountInfo struct {
	TotalEquity      float64 `json:"total_equity"`      // 账户净值
	AvailableBalance float64 `json:"available_balance"` // 可用余额
	TotalPnL         float64 `json:"total_pnl"`         // 总盈亏
	TotalPnLPct      float64 `json:"total_pnl_pct"`     // 总盈亏百分比
	MarginUsed       float64 `json:"margin_used"`       // 已用保证金
	MarginUsedPct    float64 `json:"margin_used_pct"`   // 保证金使用率
	PositionCount    int     `json:"position_count"`    // 持仓数量
}

// CandidateCoin 候选币种（来自币种池）
type CandidateCoin struct {
	Symbol  string   `json:"symbol"`
	Sources []string `json:"sources"` // 来源: "ai500" 和/或 "oi_top"
}

// OITopData 持仓量增长Top数据（用于AI决策参考）
type OITopData struct {
	Rank              int     // OI Top排名
	OIDeltaPercent    float64 // 持仓量变化百分比（1小时）
	OIDeltaValue      float64 // 持仓量变化价值
	PriceDeltaPercent float64 // 价格变化百分比
	NetLong           float64 // 净多仓
	NetShort          float64 // 净空仓
}

// Context 交易上下文（传递给AI的完整信息）
type Context struct {
	CurrentTime     string                  `json:"current_time"`
	RuntimeMinutes  int                     `json:"runtime_minutes"`
	CallCount       int                     `json:"call_count"`
	Account         AccountInfo             `json:"account"`
	Positions       []PositionInfo          `json:"positions"`
	CandidateCoins  []CandidateCoin         `json:"candidate_coins"`
	MarketDataMap   map[string]*market.Data `json:"-"` // 不序列化，但内部使用
	OITopDataMap    map[string]*OITopData   `json:"-"` // OI Top数据映射
	Performance     interface{}             `json:"-"` // 历史表现分析（logger.PerformanceAnalysis）
	BTCETHLeverage  int                     `json:"-"` // BTC/ETH杠杆倍数（从配置读取）
	AltcoinLeverage int                     `json:"-"` // 山寨币杠杆倍数（从配置读取）
}

// Decision AI的交易决策
type Decision struct {
	Symbol          string  `json:"symbol"`
	Action          string  `json:"action"` // "open_long", "open_short", "close_long", "close_short", "hold", "wait"
	Leverage        int     `json:"leverage,omitempty"`
	PositionSizeUSD float64 `json:"position_size_usd,omitempty"`
	StopLoss        float64 `json:"stop_loss,omitempty"`
	TakeProfit      float64 `json:"take_profit,omitempty"`
	Confidence      int     `json:"confidence,omitempty"` // 信心度 (0-100)
	RiskUSD         float64 `json:"risk_usd,omitempty"`   // 最大美元风险
	Reasoning       string  `json:"reasoning"`
}

// FullDecision AI的完整决策（包含思维链）
type FullDecision struct {
	UserPrompt string     `json:"user_prompt"` // 发送给AI的输入prompt
	CoTTrace   string     `json:"cot_trace"`   // 思维链分析（AI输出）
	Decisions  []Decision `json:"decisions"`   // 具体决策列表
	Timestamp  time.Time  `json:"timestamp"`
}

// GetFullDecision 获取AI的完整交易决策（批量分析所有币种和持仓）
func GetFullDecision(ctx *Context, mcpClient *mcp.Client) (*FullDecision, error) {
	// 1. 为所有币种获取市场数据
	if err := fetchMarketDataForContext(ctx); err != nil {
		return nil, fmt.Errorf("获取市场数据失败: %w", err)
	}

	// 2. 构建 System Prompt（固定规则）和 User Prompt（动态数据）
	systemPrompt := buildSystemPrompt(ctx.Account.TotalEquity, ctx.BTCETHLeverage, ctx.AltcoinLeverage)
	userPrompt := buildUserPrompt(ctx)

	// 3. 调用AI API（使用 system + user prompt）
	aiResponse, err := mcpClient.CallWithMessages(systemPrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("调用AI API失败: %w", err)
	}

	// 4. 解析AI响应
	decision, err := parseFullDecisionResponse(aiResponse, ctx.Account.TotalEquity, ctx.BTCETHLeverage, ctx.AltcoinLeverage)
	if err != nil {
		return nil, fmt.Errorf("解析AI响应失败: %w", err)
	}

	decision.Timestamp = time.Now()
	decision.UserPrompt = userPrompt // 保存输入prompt
	return decision, nil
}

// fetchMarketDataForContext 为上下文中的所有币种获取市场数据和OI数据
func fetchMarketDataForContext(ctx *Context) error {
	ctx.MarketDataMap = make(map[string]*market.Data)
	ctx.OITopDataMap = make(map[string]*OITopData)

	// 收集所有需要获取数据的币种
	symbolSet := make(map[string]bool)

	// 1. 优先获取持仓币种的数据（这是必须的）
	for _, pos := range ctx.Positions {
		symbolSet[pos.Symbol] = true
	}

	// 2. 候选币种数量根据账户状态动态调整
	maxCandidates := calculateMaxCandidates(ctx)
	for i, coin := range ctx.CandidateCoins {
		if i >= maxCandidates {
			break
		}
		symbolSet[coin.Symbol] = true
	}

	// 并发获取市场数据
	// 持仓币种集合（用于判断是否跳过OI检查）
	positionSymbols := make(map[string]bool)
	for _, pos := range ctx.Positions {
		positionSymbols[pos.Symbol] = true
	}

	for symbol := range symbolSet {
		data, err := market.Get(symbol)
		if err != nil {
			// 单个币种失败不影响整体，只记录错误
			continue
		}

		// ⚠️ 流动性过滤：持仓价值低于15M USD的币种不做（多空都不做）
		// 持仓价值 = 持仓量 × 当前价格
		// 但现有持仓必须保留（需要决策是否平仓）
		isExistingPosition := positionSymbols[symbol]
		if !isExistingPosition && data.OpenInterest != nil && data.CurrentPrice > 0 {
			// 计算持仓价值（USD）= 持仓量 × 当前价格
			oiValue := data.OpenInterest.Latest * data.CurrentPrice
			oiValueInMillions := oiValue / 1_000_000 // 转换为百万美元单位
			if oiValueInMillions < 15 {
				log.Printf("⚠️  %s 持仓价值过低(%.2fM USD < 15M)，跳过此币种 [持仓量:%.0f × 价格:%.4f]",
					symbol, oiValueInMillions, data.OpenInterest.Latest, data.CurrentPrice)
				continue
			}
		}

		ctx.MarketDataMap[symbol] = data
	}

	// 加载OI Top数据（不影响主流程）
	oiPositions, err := pool.GetOITopPositions()
	if err == nil {
		for _, pos := range oiPositions {
			// 标准化符号匹配
			symbol := pos.Symbol
			ctx.OITopDataMap[symbol] = &OITopData{
				Rank:              pos.Rank,
				OIDeltaPercent:    pos.OIDeltaPercent,
				OIDeltaValue:      pos.OIDeltaValue,
				PriceDeltaPercent: pos.PriceDeltaPercent,
				NetLong:           pos.NetLong,
				NetShort:          pos.NetShort,
			}
		}
	}

	return nil
}

// calculateMaxCandidates 根据账户状态计算需要分析的候选币种数量
func calculateMaxCandidates(ctx *Context) int {
	// 直接返回候选池的全部币种数量
	// 因为候选池已经在 auto_trader.go 中筛选过了
	// 固定分析前20个评分最高的币种（来自AI500）
	return len(ctx.CandidateCoins)
}

// buildSystemPrompt 构建 System Prompt（固定规则，可缓存）
func buildSystemPrompt(accountEquity float64, btcEthLeverage, altcoinLeverage int) string {
	// 读取系统提示词模板文件
	promptPath := filepath.Join("prompts", "system_prompt.txt")
	templateBytes, err := os.ReadFile(promptPath)
	if err != nil {
		log.Printf("⚠️  无法读取系统提示词文件 %s: %v，使用默认提示词", promptPath, err)
		return buildDefaultSystemPrompt(accountEquity, btcEthLeverage, altcoinLeverage)
	}

	template := string(templateBytes)

	// 计算动态参数
	altcoinMin := accountEquity * 0.8
	altcoinMax := accountEquity * 1.5
	btcethMin := accountEquity * 5
	btcethMax := accountEquity * 10
	exampleSize := accountEquity * 5

	// 替换占位符
	replacements := map[string]string{
		"{ALTCOIN_MIN}":      fmt.Sprintf("%.0f", altcoinMin),
		"{ALTCOIN_MAX}":      fmt.Sprintf("%.0f", altcoinMax),
		"{BTCETH_MIN}":       fmt.Sprintf("%.0f", btcethMin),
		"{BTCETH_MAX}":       fmt.Sprintf("%.0f", btcethMax),
		"{ALTCOIN_LEVERAGE}": fmt.Sprintf("%d", altcoinLeverage),
		"{BTCETH_LEVERAGE}":  fmt.Sprintf("%d", btcEthLeverage),
		"{EXAMPLE_SIZE}":     fmt.Sprintf("%.0f", exampleSize),
	}

	result := template
	for placeholder, value := range replacements {
		result = strings.ReplaceAll(result, placeholder, value)
	}

	return result
}

// buildDefaultSystemPrompt 默认系统提示词（当文件读取失败时使用）
func buildDefaultSystemPrompt(accountEquity float64, btcEthLeverage, altcoinLeverage int) string {
	var sb strings.Builder
	sb.WriteString(" ")
	
	return sb.String()
}

// buildUserPrompt 构建 User Prompt（动态数据）
func buildUserPrompt(ctx *Context) string {
	var sb strings.Builder

	// 系统状态
	sb.WriteString(fmt.Sprintf("**时间**: %s | **周期**: #%d | **运行**: %d分钟\n\n",
		ctx.CurrentTime, ctx.CallCount, ctx.RuntimeMinutes))

	// BTC 市场
	if btcData, hasBTC := ctx.MarketDataMap["BTCUSDT"]; hasBTC {
		sb.WriteString(fmt.Sprintf("**BTC**: %.2f (1h: %+.2f%%, 4h: %+.2f%%) | MACD: %.4f | RSI: %.2f\n\n",
			btcData.CurrentPrice, btcData.PriceChange1h, btcData.PriceChange4h,
			btcData.CurrentMACD, btcData.CurrentRSI7))
	}

	// 账户
	sb.WriteString(fmt.Sprintf("**账户**: 净值%.2f | 余额%.2f (%.1f%%) | 盈亏%+.2f%% | 保证金%.1f%% | 持仓%d个\n\n",
		ctx.Account.TotalEquity,
		ctx.Account.AvailableBalance,
		(ctx.Account.AvailableBalance/ctx.Account.TotalEquity)*100,
		ctx.Account.TotalPnLPct,
		ctx.Account.MarginUsedPct,
		ctx.Account.PositionCount))

	// 持仓（完整市场数据）
	if len(ctx.Positions) > 0 {
		sb.WriteString("## 当前持仓\n")
		for i, pos := range ctx.Positions {
			// 计算持仓时长
			holdingDuration := ""
			if pos.UpdateTime > 0 {
				durationMs := time.Now().UnixMilli() - pos.UpdateTime
				durationMin := durationMs / (1000 * 60) // 转换为分钟
				if durationMin < 60 {
					holdingDuration = fmt.Sprintf(" | 持仓时长%d分钟", durationMin)
				} else {
					durationHour := durationMin / 60
					durationMinRemainder := durationMin % 60
					holdingDuration = fmt.Sprintf(" | 持仓时长%d小时%d分钟", durationHour, durationMinRemainder)
				}
			}

			sb.WriteString(fmt.Sprintf("%d. %s %s | 入场价%.4f 当前价%.4f | 盈亏%+.2f%% | 杠杆%dx | 保证金%.0f | 强平价%.4f%s\n\n",
				i+1, pos.Symbol, strings.ToUpper(pos.Side),
				pos.EntryPrice, pos.MarkPrice, pos.UnrealizedPnLPct,
				pos.Leverage, pos.MarginUsed, pos.LiquidationPrice, holdingDuration))

			// 使用FormatMarketData输出完整市场数据
			if marketData, ok := ctx.MarketDataMap[pos.Symbol]; ok {
				sb.WriteString(market.Format(marketData))
				sb.WriteString("\n")
			}
		}
	} else {
		sb.WriteString("**当前持仓**: 无\n\n")
	}

	// 候选币种（完整市场数据）
	sb.WriteString(fmt.Sprintf("## 候选币种 (%d个)\n\n", len(ctx.MarketDataMap)))
	displayedCount := 0
	for _, coin := range ctx.CandidateCoins {
		marketData, hasData := ctx.MarketDataMap[coin.Symbol]
		if !hasData {
			continue
		}
		displayedCount++

		sourceTags := ""
		if len(coin.Sources) > 1 {
			sourceTags = " (AI500+OI_Top双重信号)"
		} else if len(coin.Sources) == 1 && coin.Sources[0] == "oi_top" {
			sourceTags = " (OI_Top持仓增长)"
		}

		// 使用FormatMarketData输出完整市场数据
		sb.WriteString(fmt.Sprintf("### %d. %s%s\n\n", displayedCount, coin.Symbol, sourceTags))
		sb.WriteString(market.Format(marketData))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	// 夏普比率（直接传值，不要复杂格式化）
	if ctx.Performance != nil {
		// 直接从interface{}中提取SharpeRatio
		type PerformanceData struct {
			SharpeRatio float64 `json:"sharpe_ratio"`
		}
		var perfData PerformanceData
		if jsonData, err := json.Marshal(ctx.Performance); err == nil {
			if err := json.Unmarshal(jsonData, &perfData); err == nil {
				sb.WriteString(fmt.Sprintf("## 📊 夏普比率: %.2f\n\n", perfData.SharpeRatio))
			}
		}
	}

	sb.WriteString("---\n\n")
	sb.WriteString("## 请输出决策\n\n")
	sb.WriteString("**格式要求：**\n")
	sb.WriteString("1. 先输出思维链分析（纯文本）\n")
	sb.WriteString("2. 然后输出 JSON 决策数组（必须是 `[...]` 数组格式）\n")
	sb.WriteString("3. 如果无操作，输出: `[{\"symbol\": \"MARKET\", \"action\": \"wait\", \"reasoning\": \"...\"}]`\n\n")

	return sb.String()
}

// parseFullDecisionResponse 解析AI的完整决策响应
func parseFullDecisionResponse(aiResponse string, accountEquity float64, btcEthLeverage, altcoinLeverage int) (*FullDecision, error) {
	// 1. 提取思维链
	cotTrace := extractCoTTrace(aiResponse)

	// 2. 提取JSON决策列表
	decisions, err := extractDecisions(aiResponse)
	if err != nil {
		return &FullDecision{
			CoTTrace:  cotTrace,
			Decisions: []Decision{},
		}, fmt.Errorf("提取决策失败: %w\n\n=== AI思维链分析 ===\n%s", err, cotTrace)
	}

	// 3. 验证决策
	if err := validateDecisions(decisions, accountEquity, btcEthLeverage, altcoinLeverage); err != nil {
		return &FullDecision{
			CoTTrace:  cotTrace,
			Decisions: decisions,
		}, fmt.Errorf("决策验证失败: %w\n\n=== AI思维链分析 ===\n%s", err, cotTrace)
	}

	return &FullDecision{
		CoTTrace:  cotTrace,
		Decisions: decisions,
	}, nil
}

// extractCoTTrace 提取思维链分析
func extractCoTTrace(response string) string {
	// 查找JSON数组的开始位置
	jsonStart := strings.Index(response, "[")

	if jsonStart > 0 {
		// 思维链是JSON数组之前的内容
		return strings.TrimSpace(response[:jsonStart])
	}

	// 如果找不到JSON，整个响应都是思维链
	return strings.TrimSpace(response)
}

// extractDecisions 提取JSON决策列表
func extractDecisions(response string) ([]Decision, error) {
	// 1. 尝试从代码块中提取（```json ... ```）
	if jsonStart := strings.Index(response, "```json"); jsonStart != -1 {
		jsonStart += 7 // 跳过 ```json
		if jsonEnd := strings.Index(response[jsonStart:], "```"); jsonEnd != -1 {
			response = response[jsonStart : jsonStart+jsonEnd]
		}
	} else if jsonStart := strings.Index(response, "```"); jsonStart != -1 {
		// 尝试普通代码块（```）
		jsonStart += 3
		if jsonEnd := strings.Index(response[jsonStart:], "```"); jsonEnd != -1 {
			response = response[jsonStart : jsonStart+jsonEnd]
		}
	}
	
	// 2. 查找JSON的开始位置（数组或对象）
	arrayStart := strings.Index(response, "[")
	objectStart := strings.Index(response, "{")
	
	var jsonContent string
	
	// 优先查找数组
	if arrayStart != -1 && (objectStart == -1 || arrayStart < objectStart) {
		// 找到数组
		arrayEnd := findMatchingBracket(response, arrayStart)
		if arrayEnd == -1 {
			preview := response[arrayStart:]
			if len(preview) > 500 {
				preview = preview[:500] + "..."
			}
			return nil, fmt.Errorf("无法找到JSON数组结束\n\n=== JSON数组部分 ===\n%s", preview)
		}
		jsonContent = strings.TrimSpace(response[arrayStart : arrayEnd+1])
	} else if objectStart != -1 {
		// 找到对象，尝试将其包装成数组
		objectEnd := findMatchingBrace(response, objectStart)
		if objectEnd == -1 {
			preview := response[objectStart:]
			if len(preview) > 500 {
				preview = preview[:500] + "..."
			}
			return nil, fmt.Errorf("无法找到JSON对象结束\n\n=== JSON对象部分 ===\n%s", preview)
		}
		singleObject := strings.TrimSpace(response[objectStart : objectEnd+1])
		jsonContent = "[" + singleObject + "]" // 包装成数组
		log.Printf("⚠️  AI返回了单个对象而不是数组，已自动包装")
	} else {
		// 既没有找到数组也没有找到对象
		preview := response
		if len(preview) > 500 {
			preview = preview[:500] + "..."
		}
		return nil, fmt.Errorf("无法找到JSON数组或对象起始\n\n=== AI响应预览 ===\n%s", preview)
	}

	// 3. 🔧 修复常见的JSON格式错误
	jsonContent = fixMissingQuotes(jsonContent)

	// 4. 解析JSON
	var decisions []Decision
	if err := json.Unmarshal([]byte(jsonContent), &decisions); err != nil {
		// 尝试解析为字符串数组（AI可能返回了错误格式）
		var strArray []string
		if err2 := json.Unmarshal([]byte(jsonContent), &strArray); err2 == nil {
			return nil, fmt.Errorf("AI返回了字符串数组而不是决策对象数组\n字符串内容: %v\n\n请检查AI是否按照正确格式输出（应该是包含symbol、action等字段的对象数组）", strArray)
		}
		
		return nil, fmt.Errorf("JSON解析失败: %w\n\n=== JSON内容 ===\n%s\n\n💡 提示：请确保AI输出的JSON包含以下字段：symbol, action, leverage, position_size_usd, stop_loss, take_profit, confidence, reasoning", err, jsonContent)
	}

	return decisions, nil
}

// fixMissingQuotes 替换中文引号为英文引号（避免输入法自动转换）
func fixMissingQuotes(jsonStr string) string {
	jsonStr = strings.ReplaceAll(jsonStr, "\u201c", "\"") // "
	jsonStr = strings.ReplaceAll(jsonStr, "\u201d", "\"") // "
	jsonStr = strings.ReplaceAll(jsonStr, "\u2018", "'")  // '
	jsonStr = strings.ReplaceAll(jsonStr, "\u2019", "'")  // '
	return jsonStr
}

// validateDecisions 验证所有决策（需要账户信息和杠杆配置）
func validateDecisions(decisions []Decision, accountEquity float64, btcEthLeverage, altcoinLeverage int) error {
	for i, decision := range decisions {
		if err := validateDecision(&decision, accountEquity, btcEthLeverage, altcoinLeverage); err != nil {
			return fmt.Errorf("决策 #%d 验证失败: %w", i+1, err)
		}
	}
	return nil
}

// findMatchingBracket 查找匹配的右括号
func findMatchingBracket(s string, start int) int {
	if start >= len(s) || s[start] != '[' {
		return -1
	}

	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}

	return -1
}

// findMatchingBrace 查找匹配的右大括号
func findMatchingBrace(s string, start int) int {
	if start >= len(s) || s[start] != '{' {
		return -1
	}

	depth := 0
	inString := false
	escape := false
	
	for i := start; i < len(s); i++ {
		c := s[i]
		
		// 处理字符串中的引号
		if escape {
			escape = false
			continue
		}
		
		if c == '\\' {
			escape = true
			continue
		}
		
		if c == '"' {
			inString = !inString
			continue
		}
		
		// 只在非字符串内计数大括号
		if !inString {
			switch c {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return i
				}
			}
		}
	}

	return -1
}

// validateDecision 验证单个决策的有效性
func validateDecision(d *Decision, accountEquity float64, btcEthLeverage, altcoinLeverage int) error {
	// 验证action
	validActions := map[string]bool{
		"open_long":   true,
		"open_short":  true,
		"close_long":  true,
		"close_short": true,
		"hold":        true,
		"wait":        true,
	}

	if !validActions[d.Action] {
		return fmt.Errorf("无效的action: %s", d.Action)
	}

	// 开仓操作必须提供完整参数
	if d.Action == "open_long" || d.Action == "open_short" {
		// 根据币种使用配置的杠杆上限
		maxLeverage := altcoinLeverage          // 山寨币使用配置的杠杆
		maxPositionValue := accountEquity * 1.5 // 山寨币最多1.5倍账户净值
		if d.Symbol == "BTCUSDT" || d.Symbol == "ETHUSDT" {
			maxLeverage = btcEthLeverage          // BTC和ETH使用配置的杠杆
			maxPositionValue = accountEquity * 10 // BTC/ETH最多10倍账户净值
		}

		if d.Leverage <= 0 || d.Leverage > maxLeverage {
			return fmt.Errorf("杠杆必须在1-%d之间（%s，当前配置上限%d倍）: %d", maxLeverage, d.Symbol, maxLeverage, d.Leverage)
		}
		if d.PositionSizeUSD <= 0 {
			return fmt.Errorf("仓位大小必须大于0: %.2f", d.PositionSizeUSD)
		}
		// 验证仓位价值上限（加1%容差以避免浮点数精度问题）
		tolerance := maxPositionValue * 0.01 // 1%容差
		if d.PositionSizeUSD > maxPositionValue+tolerance {
			if d.Symbol == "BTCUSDT" || d.Symbol == "ETHUSDT" {
				return fmt.Errorf("BTC/ETH单币种仓位价值不能超过%.0f USDT（10倍账户净值），实际: %.0f", maxPositionValue, d.PositionSizeUSD)
			} else {
				return fmt.Errorf("山寨币单币种仓位价值不能超过%.0f USDT（1.5倍账户净值），实际: %.0f", maxPositionValue, d.PositionSizeUSD)
			}
		}
		if d.StopLoss <= 0 || d.TakeProfit <= 0 {
			return fmt.Errorf("止损和止盈必须大于0")
		}

		// 验证止损止盈的合理性
		if d.Action == "open_long" {
			if d.StopLoss >= d.TakeProfit {
				return fmt.Errorf("做多时止损价必须小于止盈价")
			}
		} else {
			if d.StopLoss <= d.TakeProfit {
				return fmt.Errorf("做空时止损价必须大于止盈价")
			}
		}

		// 验证风险回报比（必须≥1:3）
		// 计算入场价（假设当前市价）
		var entryPrice float64
		if d.Action == "open_long" {
			// 做多：入场价在止损和止盈之间
			entryPrice = d.StopLoss + (d.TakeProfit-d.StopLoss)*0.2 // 假设在20%位置入场
		} else {
			// 做空：入场价在止损和止盈之间
			entryPrice = d.StopLoss - (d.StopLoss-d.TakeProfit)*0.2 // 假设在20%位置入场
		}

		var riskPercent, rewardPercent, riskRewardRatio float64
		if d.Action == "open_long" {
			riskPercent = (entryPrice - d.StopLoss) / entryPrice * 100
			rewardPercent = (d.TakeProfit - entryPrice) / entryPrice * 100
			if riskPercent > 0 {
				riskRewardRatio = rewardPercent / riskPercent
			}
		} else {
			riskPercent = (d.StopLoss - entryPrice) / entryPrice * 100
			rewardPercent = (entryPrice - d.TakeProfit) / entryPrice * 100
			if riskPercent > 0 {
				riskRewardRatio = rewardPercent / riskPercent
			}
		}

		// 硬约束：风险回报比必须≥3.0
		if riskRewardRatio < 3.0 {
			return fmt.Errorf("风险回报比过低(%.2f:1)，必须≥3.0:1 [风险:%.2f%% 收益:%.2f%%] [止损:%.2f 止盈:%.2f]",
				riskRewardRatio, riskPercent, rewardPercent, d.StopLoss, d.TakeProfit)
		}
	}

	return nil
}
