package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"one-api/common"
	"one-api/constant"
	"one-api/dto"
	"one-api/model"
	relaycommon "one-api/relay/common"
	relayconstant "one-api/relay/constant"
	"one-api/relay/helper"
	"one-api/service"
	"one-api/setting"
	"one-api/setting/model_setting"
	"one-api/setting/operation_setting"
	"strings"
	"time"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/shopspring/decimal"

	"github.com/gin-gonic/gin"
)

func getAndValidateTextRequest(c *gin.Context, relayInfo *relaycommon.RelayInfo) (*dto.GeneralOpenAIRequest, error) {
	textRequest := &dto.GeneralOpenAIRequest{}
	err := common.UnmarshalBodyReusable(c, textRequest)
	if err != nil {
		return nil, err
	}
	if relayInfo.RelayMode == relayconstant.RelayModeModerations && textRequest.Model == "" {
		textRequest.Model = "text-moderation-latest"
	}
	if relayInfo.RelayMode == relayconstant.RelayModeEmbeddings && textRequest.Model == "" {
		textRequest.Model = c.Param("model")
	}

	if textRequest.MaxTokens > math.MaxInt32/2 {
		return nil, errors.New("max_tokens is invalid")
	}
	if textRequest.Model == "" {
		return nil, errors.New("model is required")
	}
	switch relayInfo.RelayMode {
	case relayconstant.RelayModeCompletions:
		if textRequest.Prompt == "" {
			return nil, errors.New("field prompt is required")
		}
	case relayconstant.RelayModeChatCompletions:
		if len(textRequest.Messages) == 0 {
			return nil, errors.New("field messages is required")
		}
	case relayconstant.RelayModeEmbeddings:
	case relayconstant.RelayModeModerations:
		if textRequest.Input == nil || textRequest.Input == "" {
			return nil, errors.New("field input is required")
		}
	case relayconstant.RelayModeEdits:
		if textRequest.Instruction == "" {
			return nil, errors.New("field instruction is required")
		}
	}
	relayInfo.IsStream = textRequest.Stream
	return textRequest, nil
}

func TextHelper(c *gin.Context) (openaiErr *dto.OpenAIErrorWithStatusCode) {
	reqId := c.GetString("request_id")
	common.LogInfo(c, fmt.Sprintf("[%s] TextHelper开始处理请求", reqId))

	relayInfo := relaycommon.GenRelayInfo(c)
	common.LogInfo(c, fmt.Sprintf("[%s] 生成中继信息: 用户ID=%d, 渠道ID=%d, 模型=%s, 中继模式=%d",
		reqId, relayInfo.UserId, relayInfo.ChannelId, relayInfo.OriginModelName, relayInfo.RelayMode))

	// get & validate textRequest 获取并验证文本请求
	textRequest, err := getAndValidateTextRequest(c, relayInfo)
	if err != nil {
		common.LogError(c, fmt.Sprintf("[%s] getAndValidateTextRequest failed: %s", reqId, err.Error()))
		return service.OpenAIErrorWrapperLocal(err, "invalid_text_request", http.StatusBadRequest)
	}
	common.LogInfo(c, fmt.Sprintf("[%s] 文本请求验证成功，模型=%s, 流式=%v", reqId, textRequest.Model, relayInfo.IsStream))

	if setting.ShouldCheckPromptSensitive() {
		common.LogInfo(c, fmt.Sprintf("[%s] 开始检查敏感词", reqId))
		words, err := checkRequestSensitive(textRequest, relayInfo)
		if err != nil {
			common.LogWarn(c, fmt.Sprintf("[%s] 用户敏感词检测: %s", reqId, strings.Join(words, ", ")))
			return service.OpenAIErrorWrapperLocal(err, "sensitive_words_detected", http.StatusBadRequest)
		}
		common.LogInfo(c, fmt.Sprintf("[%s] 敏感词检查通过", reqId))
	}

	err = helper.ModelMappedHelper(c, relayInfo)
	if err != nil {
		common.LogError(c, fmt.Sprintf("[%s] 模型映射错误: %s", reqId, err.Error()))
		return service.OpenAIErrorWrapperLocal(err, "model_mapped_error", http.StatusInternalServerError)
	}
	common.LogInfo(c, fmt.Sprintf("[%s] 模型映射完成: 源模型=%s, 上游模型=%s", reqId, relayInfo.OriginModelName, relayInfo.UpstreamModelName))

	textRequest.Model = relayInfo.UpstreamModelName

	// 获取 promptTokens，如果上下文中已经存在，则直接使用
	var promptTokens int
	if value, exists := c.Get("prompt_tokens"); exists {
		promptTokens = value.(int)
		relayInfo.PromptTokens = promptTokens
		common.LogInfo(c, fmt.Sprintf("[%s] 从上下文获取promptTokens=%d", reqId, promptTokens))
	} else {
		promptTokens, err = getPromptTokens(textRequest, relayInfo)
		// count messages token error 计算promptTokens错误
		if err != nil {
			common.LogError(c, fmt.Sprintf("[%s] 计算token失败: %s", reqId, err.Error()))
			return service.OpenAIErrorWrapper(err, "count_token_messages_failed", http.StatusInternalServerError)
		}
		c.Set("prompt_tokens", promptTokens)
		common.LogInfo(c, fmt.Sprintf("[%s] 计算获取promptTokens=%d", reqId, promptTokens))
	}

	priceData, err := helper.ModelPriceHelper(c, relayInfo, promptTokens, int(math.Max(float64(textRequest.MaxTokens), float64(textRequest.MaxCompletionTokens))))
	if err != nil {
		common.LogError(c, fmt.Sprintf("[%s] 模型价格计算错误: %s", reqId, err.Error()))
		return service.OpenAIErrorWrapperLocal(err, "model_price_error", http.StatusInternalServerError)
	}
	common.LogInfo(c, fmt.Sprintf("[%s] 价格计算完成: 模型价格=%f, 组倍率=%f, 使用价格=%v",
		reqId, priceData.ModelPrice, priceData.GroupRatio, priceData.UsePrice))

	// pre-consume quota 预消耗配额
	preConsumedQuota, userQuota, openaiErr := preConsumeQuota(c, priceData.ShouldPreConsumedQuota, relayInfo)
	if openaiErr != nil {
		common.LogError(c, fmt.Sprintf("[%s] 预消耗配额失败: %s", reqId, openaiErr.Error.Message))
		return openaiErr
	}
	common.LogInfo(c, fmt.Sprintf("[%s] 预消耗配额完成: 预消耗=%d, 用户余额=%d", reqId, preConsumedQuota, userQuota))

	defer func() {
		if openaiErr != nil {
			common.LogInfo(c, fmt.Sprintf("[%s] 请求失败，退还预消耗配额: %d", reqId, preConsumedQuota))
			returnPreConsumedQuota(c, relayInfo, userQuota, preConsumedQuota)
		}
	}()
	includeUsage := false
	// 判断用户是否需要返回使用情况
	if textRequest.StreamOptions != nil && textRequest.StreamOptions.IncludeUsage {
		includeUsage = true
	}

	// 如果不支持StreamOptions，将StreamOptions设置为nil
	if !relayInfo.SupportStreamOptions || !textRequest.Stream {
		textRequest.StreamOptions = nil
	} else {
		// 如果支持StreamOptions，且请求中没有设置StreamOptions，根据配置文件设置StreamOptions
		if constant.ForceStreamOption {
			textRequest.StreamOptions = &dto.StreamOptions{
				IncludeUsage: true,
			}
		}
	}

	if includeUsage {
		relayInfo.ShouldIncludeUsage = true
	}

	adaptor := GetAdaptor(relayInfo.ApiType)
	if adaptor == nil {
		common.LogError(c, fmt.Sprintf("[%s] 无效的API类型: %d", reqId, relayInfo.ApiType))
		return service.OpenAIErrorWrapperLocal(fmt.Errorf("invalid api type: %d", relayInfo.ApiType), "invalid_api_type", http.StatusBadRequest)
	}
	common.LogInfo(c, fmt.Sprintf("[%s] 获取适配器成功: API类型=%d", reqId, relayInfo.ApiType))

	adaptor.Init(relayInfo)
	var requestBody io.Reader

	if model_setting.GetGlobalSettings().PassThroughRequestEnabled {
		body, err := common.GetRequestBody(c)
		if err != nil {
			common.LogError(c, fmt.Sprintf("[%s] 获取请求体失败: %s", reqId, err.Error()))
			return service.OpenAIErrorWrapperLocal(err, "get_request_body_failed", http.StatusInternalServerError)
		}
		requestBody = bytes.NewBuffer(body)
		common.LogInfo(c, fmt.Sprintf("[%s] 透传请求体模式", reqId))
	} else {
		convertedRequest, err := adaptor.ConvertOpenAIRequest(c, relayInfo, textRequest)
		if err != nil {
			common.LogError(c, fmt.Sprintf("[%s] 转换请求失败: %s", reqId, err.Error()))
			return service.OpenAIErrorWrapperLocal(err, "convert_request_failed", http.StatusInternalServerError)
		}
		common.LogInfo(c, fmt.Sprintf("[%s] 请求转换成功", reqId))

		jsonData, err := json.Marshal(convertedRequest)
		if err != nil {
			common.LogError(c, fmt.Sprintf("[%s] JSON序列化失败: %s", reqId, err.Error()))
			return service.OpenAIErrorWrapperLocal(err, "json_marshal_failed", http.StatusInternalServerError)
		}

		// apply param override
		if len(relayInfo.ParamOverride) > 0 {
			common.LogInfo(c, fmt.Sprintf("[%s] 应用参数覆盖，参数数量=%d", reqId, len(relayInfo.ParamOverride)))
			reqMap := make(map[string]interface{})
			err = json.Unmarshal(jsonData, &reqMap)
			if err != nil {
				common.LogError(c, fmt.Sprintf("[%s] 参数覆盖解析失败: %s", reqId, err.Error()))
				return service.OpenAIErrorWrapperLocal(err, "param_override_unmarshal_failed", http.StatusInternalServerError)
			}
			for key, value := range relayInfo.ParamOverride {
				reqMap[key] = value
			}
			jsonData, err = json.Marshal(reqMap)
			if err != nil {
				common.LogError(c, fmt.Sprintf("[%s] 参数覆盖序列化失败: %s", reqId, err.Error()))
				return service.OpenAIErrorWrapperLocal(err, "param_override_marshal_failed", http.StatusInternalServerError)
			}
		}
		common.LogInfo(c, fmt.Sprintf("requestBody: %s", string(jsonData)))
		requestBody = bytes.NewBuffer(jsonData)
	}

	common.LogInfo(c, fmt.Sprintf("[%s] 开始发送请求", reqId))
	var httpResp *http.Response
	resp, err := adaptor.DoRequest(c, relayInfo, requestBody)
	if err != nil {
		common.LogError(c, fmt.Sprintf("[%s] 请求失败: %s", reqId, err.Error()))
		return service.OpenAIErrorWrapper(err, "do_request_failed", http.StatusInternalServerError)
	}
	common.LogInfo(c, fmt.Sprintf("[%s] 请求发送成功", reqId))

	statusCodeMappingStr := c.GetString("status_code_mapping")

	if resp != nil {
		httpResp = resp.(*http.Response)
		relayInfo.IsStream = relayInfo.IsStream || strings.HasPrefix(httpResp.Header.Get("Content-Type"), "text/event-stream")
		common.LogInfo(c, fmt.Sprintf("[%s] 收到响应: 状态码=%d, 内容类型=%s, 流式=%v",
			reqId, httpResp.StatusCode, httpResp.Header.Get("Content-Type"), relayInfo.IsStream))

		if httpResp.StatusCode != http.StatusOK {
			openaiErr = service.RelayErrorHandler(httpResp, false)
			// reset status code 重置状态码
			service.ResetStatusCode(openaiErr, statusCodeMappingStr)
			common.LogError(c, fmt.Sprintf("[%s] 响应状态码错误: %d, 错误=%s",
				reqId, httpResp.StatusCode, openaiErr.Error.Message))
			return openaiErr
		}
	}

	common.LogInfo(c, fmt.Sprintf("[%s] 开始处理响应", reqId))
	usage, openaiErr := adaptor.DoResponse(c, httpResp, relayInfo)
	if openaiErr != nil {
		// reset status code 重置状态码
		service.ResetStatusCode(openaiErr, statusCodeMappingStr)
		common.LogError(c, fmt.Sprintf("[%s] 处理响应失败: %s", reqId, openaiErr.Error.Message))
		return openaiErr
	}
	common.LogInfo(c, fmt.Sprintf("[%s] 响应处理成功", reqId))

	if strings.HasPrefix(relayInfo.OriginModelName, "gpt-4o-audio") {
		common.LogInfo(c, fmt.Sprintf("[%s] 音频模型消费配额", reqId))
		service.PostAudioConsumeQuota(c, relayInfo, usage.(*dto.Usage), preConsumedQuota, userQuota, priceData, "")
	} else {
		common.LogInfo(c, fmt.Sprintf("[%s] 消费配额: promptTokens=%d, completionTokens=%d",
			reqId, usage.(*dto.Usage).PromptTokens, usage.(*dto.Usage).CompletionTokens))
		postConsumeQuota(c, relayInfo, usage.(*dto.Usage), preConsumedQuota, userQuota, priceData, "")
	}

	common.LogInfo(c, fmt.Sprintf("[%s] TextHelper处理完成", reqId))
	return nil
}

func getPromptTokens(textRequest *dto.GeneralOpenAIRequest, info *relaycommon.RelayInfo) (int, error) {
	var promptTokens int
	var err error
	switch info.RelayMode {
	case relayconstant.RelayModeChatCompletions:
		promptTokens, err = service.CountTokenChatRequest(info, *textRequest)
	case relayconstant.RelayModeCompletions:
		promptTokens, err = service.CountTokenInput(textRequest.Prompt, textRequest.Model)
	case relayconstant.RelayModeModerations:
		promptTokens, err = service.CountTokenInput(textRequest.Input, textRequest.Model)
	case relayconstant.RelayModeEmbeddings:
		promptTokens, err = service.CountTokenInput(textRequest.Input, textRequest.Model)
	default:
		err = errors.New("unknown relay mode")
		promptTokens = 0
	}
	info.PromptTokens = promptTokens
	return promptTokens, err
}

func checkRequestSensitive(textRequest *dto.GeneralOpenAIRequest, info *relaycommon.RelayInfo) ([]string, error) {
	var err error
	var words []string
	switch info.RelayMode {
	case relayconstant.RelayModeChatCompletions:
		words, err = service.CheckSensitiveMessages(textRequest.Messages)
	case relayconstant.RelayModeCompletions:
		words, err = service.CheckSensitiveInput(textRequest.Prompt)
	case relayconstant.RelayModeModerations:
		words, err = service.CheckSensitiveInput(textRequest.Input)
	case relayconstant.RelayModeEmbeddings:
		words, err = service.CheckSensitiveInput(textRequest.Input)
	}
	return words, err
}

// 预扣费并返回用户剩余配额
func preConsumeQuota(c *gin.Context, preConsumedQuota int, relayInfo *relaycommon.RelayInfo) (int, int, *dto.OpenAIErrorWithStatusCode) {
	userQuota, err := model.GetUserQuota(relayInfo.UserId, false)
	if err != nil {
		return 0, 0, service.OpenAIErrorWrapperLocal(err, "get_user_quota_failed", http.StatusInternalServerError)
	}
	if userQuota <= 0 {
		return 0, 0, service.OpenAIErrorWrapperLocal(errors.New("user quota is not enough"), "insufficient_user_quota", http.StatusForbidden)
	}
	if userQuota-preConsumedQuota < 0 {
		return 0, 0, service.OpenAIErrorWrapperLocal(fmt.Errorf("chat pre-consumed quota failed, user quota: %s, need quota: %s", common.FormatQuota(userQuota), common.FormatQuota(preConsumedQuota)), "insufficient_user_quota", http.StatusForbidden)
	}
	relayInfo.UserQuota = userQuota
	if userQuota > 100*preConsumedQuota {
		// 用户额度充足，判断令牌额度是否充足
		if !relayInfo.TokenUnlimited {
			// 非无限令牌，判断令牌额度是否充足
			tokenQuota := c.GetInt("token_quota")
			if tokenQuota > 100*preConsumedQuota {
				// 令牌额度充足，信任令牌
				preConsumedQuota = 0
				common.LogInfo(c, fmt.Sprintf("user %d quota %s and token %d quota %d are enough, trusted and no need to pre-consume", relayInfo.UserId, common.FormatQuota(userQuota), relayInfo.TokenId, tokenQuota))
			}
		} else {
			// in this case, we do not pre-consume quota
			// because the user has enough quota
			preConsumedQuota = 0
			common.LogInfo(c, fmt.Sprintf("user %d with unlimited token has enough quota %s, trusted and no need to pre-consume", relayInfo.UserId, common.FormatQuota(userQuota)))
		}
	}

	if preConsumedQuota > 0 {
		err := service.PreConsumeTokenQuota(relayInfo, preConsumedQuota)
		if err != nil {
			return 0, 0, service.OpenAIErrorWrapperLocal(err, "pre_consume_token_quota_failed", http.StatusForbidden)
		}
		err = model.DecreaseUserQuota(relayInfo.UserId, preConsumedQuota)
		if err != nil {
			return 0, 0, service.OpenAIErrorWrapperLocal(err, "decrease_user_quota_failed", http.StatusInternalServerError)
		}
	}
	return preConsumedQuota, userQuota, nil
}

func returnPreConsumedQuota(c *gin.Context, relayInfo *relaycommon.RelayInfo, userQuota int, preConsumedQuota int) {
	if preConsumedQuota != 0 {
		gopool.Go(func() {
			relayInfoCopy := *relayInfo

			err := service.PostConsumeQuota(&relayInfoCopy, -preConsumedQuota, 0, false)
			if err != nil {
				common.SysError("error return pre-consumed quota: " + err.Error())
			}
		})
	}
}

func postConsumeQuota(ctx *gin.Context, relayInfo *relaycommon.RelayInfo,
	usage *dto.Usage, preConsumedQuota int, userQuota int, priceData helper.PriceData, extraContent string) {
	if usage == nil {
		usage = &dto.Usage{
			PromptTokens:     relayInfo.PromptTokens,
			CompletionTokens: 0,
			TotalTokens:      relayInfo.PromptTokens,
		}
		extraContent += "（可能是请求出错）"
	}
	useTimeSeconds := time.Now().Unix() - relayInfo.StartTime.Unix()
	promptTokens := usage.PromptTokens
	cacheTokens := usage.PromptTokensDetails.CachedTokens
	imageTokens := usage.PromptTokensDetails.ImageTokens
	completionTokens := usage.CompletionTokens
	modelName := relayInfo.OriginModelName

	tokenName := ctx.GetString("token_name")
	completionRatio := priceData.CompletionRatio
	cacheRatio := priceData.CacheRatio
	imageRatio := priceData.ImageRatio
	modelRatio := priceData.ModelRatio
	groupRatio := priceData.GroupRatio
	modelPrice := priceData.ModelPrice

	// Convert values to decimal for precise calculation
	dPromptTokens := decimal.NewFromInt(int64(promptTokens))
	dCacheTokens := decimal.NewFromInt(int64(cacheTokens))
	dImageTokens := decimal.NewFromInt(int64(imageTokens))
	dCompletionTokens := decimal.NewFromInt(int64(completionTokens))
	dCompletionRatio := decimal.NewFromFloat(completionRatio)
	dCacheRatio := decimal.NewFromFloat(cacheRatio)
	dImageRatio := decimal.NewFromFloat(imageRatio)
	dModelRatio := decimal.NewFromFloat(modelRatio)
	dGroupRatio := decimal.NewFromFloat(groupRatio)
	dModelPrice := decimal.NewFromFloat(modelPrice)
	dQuotaPerUnit := decimal.NewFromFloat(common.QuotaPerUnit)

	ratio := dModelRatio.Mul(dGroupRatio)

	// openai web search 工具计费
	var dWebSearchQuota decimal.Decimal
	var webSearchPrice float64
	if relayInfo.ResponsesUsageInfo != nil {
		if webSearchTool, exists := relayInfo.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolWebSearchPreview]; exists && webSearchTool.CallCount > 0 {
			// 计算 web search 调用的配额 (配额 = 价格 * 调用次数 / 1000 * 分组倍率)
			webSearchPrice = operation_setting.GetWebSearchPricePerThousand(modelName, webSearchTool.SearchContextSize)
			dWebSearchQuota = decimal.NewFromFloat(webSearchPrice).
				Mul(decimal.NewFromInt(int64(webSearchTool.CallCount))).
				Div(decimal.NewFromInt(1000)).Mul(dGroupRatio).Mul(dQuotaPerUnit)
			extraContent += fmt.Sprintf("Web Search 调用 %d 次，上下文大小 %s，调用花费 $%s",
				webSearchTool.CallCount, webSearchTool.SearchContextSize, dWebSearchQuota.String())
		}
	}
	// file search tool 计费
	var dFileSearchQuota decimal.Decimal
	var fileSearchPrice float64
	if relayInfo.ResponsesUsageInfo != nil {
		if fileSearchTool, exists := relayInfo.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolFileSearch]; exists && fileSearchTool.CallCount > 0 {
			fileSearchPrice = operation_setting.GetFileSearchPricePerThousand()
			dFileSearchQuota = decimal.NewFromFloat(fileSearchPrice).
				Mul(decimal.NewFromInt(int64(fileSearchTool.CallCount))).
				Div(decimal.NewFromInt(1000)).Mul(dGroupRatio).Mul(dQuotaPerUnit)
			extraContent += fmt.Sprintf("File Search 调用 %d 次，调用花费 $%s",
				fileSearchTool.CallCount, dFileSearchQuota.String())
		}
	}

	var quotaCalculateDecimal decimal.Decimal
	if !priceData.UsePrice {
		nonCachedTokens := dPromptTokens.Sub(dCacheTokens)
		cachedTokensWithRatio := dCacheTokens.Mul(dCacheRatio)

		promptQuota := nonCachedTokens.Add(cachedTokensWithRatio)
		if imageTokens > 0 {
			nonImageTokens := dPromptTokens.Sub(dImageTokens)
			imageTokensWithRatio := dImageTokens.Mul(dImageRatio)
			promptQuota = nonImageTokens.Add(imageTokensWithRatio)
		}

		completionQuota := dCompletionTokens.Mul(dCompletionRatio)

		quotaCalculateDecimal = promptQuota.Add(completionQuota).Mul(ratio)

		if !ratio.IsZero() && quotaCalculateDecimal.LessThanOrEqual(decimal.Zero) {
			quotaCalculateDecimal = decimal.NewFromInt(1)
		}
	} else {
		quotaCalculateDecimal = dModelPrice.Mul(dQuotaPerUnit).Mul(dGroupRatio)
	}
	// 添加 responses tools call 调用的配额
	quotaCalculateDecimal = quotaCalculateDecimal.Add(dWebSearchQuota)
	quotaCalculateDecimal = quotaCalculateDecimal.Add(dFileSearchQuota)

	quota := int(quotaCalculateDecimal.Round(0).IntPart())
	totalTokens := promptTokens + completionTokens

	var logContent string
	if !priceData.UsePrice {
		logContent = fmt.Sprintf("模型倍率 %.2f，补全倍率 %.2f，分组倍率 %.2f", modelRatio, completionRatio, groupRatio)
	} else {
		logContent = fmt.Sprintf("模型价格 %.2f，分组倍率 %.2f", modelPrice, groupRatio)
	}

	// record all the consume log even if quota is 0
	if totalTokens == 0 {
		// in this case, must be some error happened
		// we cannot just return, because we may have to return the pre-consumed quota
		quota = 0
		logContent += fmt.Sprintf("（可能是上游超时）")
		common.LogError(ctx, fmt.Sprintf("total tokens is 0, cannot consume quota, userId %d, channelId %d, "+
			"tokenId %d, model %s， pre-consumed quota %d", relayInfo.UserId, relayInfo.ChannelId, relayInfo.TokenId, modelName, preConsumedQuota))
	} else {
		model.UpdateUserUsedQuotaAndRequestCount(relayInfo.UserId, quota)
		model.UpdateChannelUsedQuota(relayInfo.ChannelId, quota)
	}

	quotaDelta := quota - preConsumedQuota
	if quotaDelta != 0 {
		err := service.PostConsumeQuota(relayInfo, quotaDelta, preConsumedQuota, true)
		if err != nil {
			common.LogError(ctx, "error consuming token remain quota: "+err.Error())
		}
	}

	logModel := modelName
	if strings.HasPrefix(logModel, "gpt-4-gizmo") {
		logModel = "gpt-4-gizmo-*"
		logContent += fmt.Sprintf("，模型 %s", modelName)
	}
	if strings.HasPrefix(logModel, "gpt-4o-gizmo") {
		logModel = "gpt-4o-gizmo-*"
		logContent += fmt.Sprintf("，模型 %s", modelName)
	}
	if extraContent != "" {
		logContent += ", " + extraContent
	}
	other := service.GenerateTextOtherInfo(ctx, relayInfo, modelRatio, groupRatio, completionRatio, cacheTokens, cacheRatio, modelPrice)
	if imageTokens != 0 {
		other["image"] = true
		other["image_ratio"] = imageRatio
		other["image_output"] = imageTokens
	}
	if !dWebSearchQuota.IsZero() && relayInfo.ResponsesUsageInfo != nil {
		if webSearchTool, exists := relayInfo.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolWebSearchPreview]; exists {
			other["web_search"] = true
			other["web_search_call_count"] = webSearchTool.CallCount
			other["web_search_price"] = webSearchPrice
		}
	}
	if !dFileSearchQuota.IsZero() && relayInfo.ResponsesUsageInfo != nil {
		if fileSearchTool, exists := relayInfo.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolFileSearch]; exists {
			other["file_search"] = true
			other["file_search_call_count"] = fileSearchTool.CallCount
			other["file_search_price"] = fileSearchPrice
		}
	}
	model.RecordConsumeLog(ctx, relayInfo.UserId, relayInfo.ChannelId, promptTokens, completionTokens, logModel,
		tokenName, quota, logContent, relayInfo.TokenId, userQuota, int(useTimeSeconds), relayInfo.IsStream, relayInfo.Group, other)
}
