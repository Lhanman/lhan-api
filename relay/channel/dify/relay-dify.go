package dify

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"one-api/common"
	"one-api/constant"
	"one-api/dto"
	relaycommon "one-api/relay/common"
	"one-api/relay/helper"
	"one-api/service"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

func uploadDifyFile(c *gin.Context, info *relaycommon.RelayInfo, user string, media dto.MediaContent) *DifyFile {
	common.SysLog(fmt.Sprintf("[Dify] 开始上传文件, baseUrl: %s, mediaType: %s", info.BaseUrl, media.Type))
	uploadUrl := fmt.Sprintf("%s/v1/files/upload", info.BaseUrl)
	switch media.Type {
	case dto.ContentTypeImageURL:
		// Decode base64 data
		imageMedia := media.GetImageMedia()
		base64Data := imageMedia.Url
		common.SysLog(fmt.Sprintf("[Dify] 处理图片数据, mimeType: %s", imageMedia.MimeType))
		// Remove base64 prefix if exists (e.g., "data:image/jpeg;base64,")
		if idx := strings.Index(base64Data, ","); idx != -1 {
			base64Data = base64Data[idx+1:]
			common.SysLog("[Dify] 移除base64前缀")
		}

		// Decode base64 string
		decodedData, err := base64.StdEncoding.DecodeString(base64Data)
		if err != nil {
			common.SysError("[Dify] failed to decode base64: " + err.Error())
			return nil
		}
		common.SysLog(fmt.Sprintf("[Dify] base64解码完成, 数据大小: %d bytes", len(decodedData)))

		// Create temporary file
		tempFile, err := os.CreateTemp("", "dify-upload-*")
		if err != nil {
			common.SysError("[Dify] failed to create temp file: " + err.Error())
			return nil
		}
		common.SysLog(fmt.Sprintf("[Dify] 创建临时文件: %s", tempFile.Name()))
		defer tempFile.Close()
		defer os.Remove(tempFile.Name())

		// Write decoded data to temp file
		if _, err := tempFile.Write(decodedData); err != nil {
			common.SysError("[Dify] failed to write to temp file: " + err.Error())
			return nil
		}
		common.SysLog("[Dify] 已写入数据到临时文件")

		// Create multipart form
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)

		// Add user field (--form 'user=liujiahao10570' 格式)
		if err := writer.WriteField("user", user); err != nil {
			common.SysError("[Dify] failed to add user field: " + err.Error())
			return nil
		}
		common.SysLog(fmt.Sprintf("[Dify] 添加用户字段: %s", user))

		// Create form file with proper mime type
		mimeType := imageMedia.MimeType
		if mimeType == "" {
			mimeType = "image/png" // default mime type
			common.SysLog("[Dify] 使用默认MIME类型: image/png")
		}

		// Create form file
		part, err := writer.CreateFormFileNew("file", fmt.Sprintf("image.%s", strings.TrimPrefix(mimeType, "image/")), mimeType)
		if err != nil {
			common.SysError("[Dify] failed to create form file: " + err.Error())
			return nil
		}
		common.SysLog(fmt.Sprintf("[Dify] 创建表单文件:type=%s", mimeType))

		// Copy file content to form
		if _, err = io.Copy(part, bytes.NewReader(decodedData)); err != nil {
			common.SysError("[Dify] failed to copy file content: " + err.Error())
			return nil
		}
		common.SysLog("[Dify] 复制文件内容到表单完成")
		writer.Close()

		// Create HTTP request
		req, err := http.NewRequest("POST", uploadUrl, body)
		if err != nil {
			common.SysError("[Dify] failed to create request: " + err.Error())
			return nil
		}

		req.Header.Set("Content-Type", writer.FormDataContentType())
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", info.ApiKey))
		common.SysLog(fmt.Sprintf("[Dify] 创建HTTP请求: %s", uploadUrl))

		// Send request
		client := service.GetImpatientHttpClient()
		common.SysLog("[Dify] 发送文件上传请求... header ：" + fmt.Sprintf("%+v", req.Header))
		resp, err := client.Do(req)
		if err != nil {
			common.SysError("[Dify] failed to send request: " + err.Error())
			return nil
		}
		common.SysLog(fmt.Sprintf("[Dify] 收到响应状态码: %d", resp.StatusCode))
		defer resp.Body.Close()

		// 读取响应体内容
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			common.SysError("[Dify] failed to read response body: " + err.Error())
			return nil
		}
		common.SysLog(fmt.Sprintf("[Dify] 响应内容: %s", string(bodyBytes)))

		// 重新创建一个新的reader，给后续的json解析使用
		resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

		// Parse response
		var result struct {
			Id string `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			common.SysError("[Dify] failed to decode response: " + err.Error())
			return nil
		}
		common.SysLog(fmt.Sprintf("[Dify] 文件上传成功, ID: %s", result.Id))

		return &DifyFile{
			UploadFileId: result.Id,
			Type:         "image",
			TransferMode: "local_file",
		}
	}
	common.SysLog("[Dify] 不支持的媒体类型")
	return nil
}

func requestOpenAI2Dify(c *gin.Context, info *relaycommon.RelayInfo, request dto.GeneralOpenAIRequest) *DifyChatRequest {
	common.SysLog(fmt.Sprintf("[Dify] 开始处理OpenAI到Dify请求转换, 消息数量: %d", len(request.Messages)))
	difyReq := DifyChatRequest{
		Inputs:           make(map[string]interface{}),
		AutoGenerateName: true,
	}

	override := c.GetStringMap("param_override")
	inputs, ok := override["inputs"].(map[string]interface{})
	common.SysLog("[Dify] override: " + fmt.Sprintf("%+v", override) + ", inputs: " + fmt.Sprintf("%+v", inputs))
	if ok && inputs != nil {
		difyReq.Inputs = inputs
		common.SysLog("[Dify] 使用覆盖的inputs")
	} else {
		difyReq.Inputs = make(map[string]interface{})
		common.SysLog("[Dify] 使用默认inputs")
	}
	user := "liujiahao10570"
	common.SysLog("[Dify] user: " + user + ", inputs : " + fmt.Sprintf("%+v", difyReq.Inputs))
	difyReq.User = user

	files := make([]DifyFile, 0)
	var content strings.Builder
	for i, message := range request.Messages {
		common.SysLog(fmt.Sprintf("[Dify] 处理消息 #%d, 角色: %s", i+1, message.Role))
		if message.Role == "system" {
			content.WriteString("SYSTEM: \n" + message.StringContent() + "\n")
			common.SysLog("[Dify] 添加系统消息")
		} else if message.Role == "assistant" {
			content.WriteString("ASSISTANT: \n" + message.StringContent() + "\n")
			common.SysLog("[Dify] 添加助手消息")
		} else {
			parseContent := message.ParseContent()
			common.SysLog(fmt.Sprintf("[Dify] 解析用户消息, 内容数量: %d", len(parseContent)))
			for j, mediaContent := range parseContent {
				switch mediaContent.Type {
				case dto.ContentTypeText:
					content.WriteString("USER: \n" + mediaContent.Text + "\n")
					common.SysLog(fmt.Sprintf("[Dify] 添加用户文本 #%d", j+1))
				case dto.ContentTypeImageURL:
					common.SysLog(fmt.Sprintf("[Dify] 处理图片 #%d", j+1))
					media := mediaContent.GetImageMedia()
					var file *DifyFile
					if media.IsRemoteImage() {
						common.SysLog("[Dify] 处理远程图片: " + media.Url)
						file = &DifyFile{}
						mimeType := media.MimeType
						if mimeType == "" {
							mimeType = "image/jpeg" // default mime type
							common.SysLog("[Dify] 远程图片使用默认MIME类型: image/jpeg")
						}
						file.Type = mimeType
						file.TransferMode = "remote_url"
						file.URL = media.Url
					} else {
						common.SysLog("[Dify] 处理本地图片")
						file = uploadDifyFile(c, info, difyReq.User, mediaContent)
					}
					if file != nil {
						files = append(files, *file)
						common.SysLog(fmt.Sprintf("[Dify] 添加文件到列表, 现有文件数: %d", len(files)))
					} else {
						common.SysLog("[Dify] 文件处理失败，未添加到列表")
					}
				}
			}
		}
	}
	difyReq.Query = content.String()
	difyReq.Files = files
	mode := "streaming"
	// if request.Stream {
	// 	mode = "streaming"
	// }
	common.SysLog(fmt.Sprintf("[Dify] 请求构建完成, 查询长度: %d, 文件数量: %d, 模式: %s",
		len(difyReq.Query), len(difyReq.Files), mode))
	difyReq.ResponseMode = mode
	return &difyReq
}

func streamResponseDify2OpenAI(difyResponse DifyChunkChatCompletionResponse) *dto.ChatCompletionsStreamResponse {
	common.SysLog(fmt.Sprintf("[Dify] 处理流式响应, 事件: %s", difyResponse.Event))
	response := dto.ChatCompletionsStreamResponse{
		Object:  "chat.completion.chunk",
		Created: common.GetTimestamp(),
		Model:   "dify",
	}
	var choice dto.ChatCompletionsStreamResponseChoice
	if strings.HasPrefix(difyResponse.Event, "workflow_") {
		common.SysLog(fmt.Sprintf("[Dify] 处理工作流事件: %s, ID: %s",
			difyResponse.Event, difyResponse.Data.WorkflowId))
		if constant.DifyDebug {
			text := "Workflow: " + difyResponse.Data.WorkflowId
			if difyResponse.Event == "workflow_finished" {
				text += " " + difyResponse.Data.Status
			}
			choice.Delta.SetReasoningContent(text + "\n")
			common.SysLog(fmt.Sprintf("[Dify] 设置推理内容: %s", text))
		}
	} else if strings.HasPrefix(difyResponse.Event, "node_") {
		common.SysLog(fmt.Sprintf("[Dify] 处理节点事件: %s, 类型: %s",
			difyResponse.Event, difyResponse.Data.NodeType))
		if constant.DifyDebug {
			text := "Node: " + difyResponse.Data.NodeType
			if difyResponse.Event == "node_finished" {
				text += " " + difyResponse.Data.Status
			}
			choice.Delta.SetReasoningContent(text + "\n")
			common.SysLog(fmt.Sprintf("[Dify] 设置推理内容: %s", text))
		}
	} else if difyResponse.Event == "message" || difyResponse.Event == "agent_message" {
		answerLength := len(difyResponse.Answer)
		displayAnswer := difyResponse.Answer
		if answerLength > 50 {
			displayAnswer = displayAnswer[:50] + "..."
		}
		common.SysLog(fmt.Sprintf("[Dify] 处理消息事件, 消息长度: %d, 内容: %s",
			answerLength, displayAnswer))

		if difyResponse.Answer == "<details style=\"color:gray;background-color: #f8f8f8;padding: 8px;border-radius: 4px;\" open> <summary> Thinking... </summary>\n" {
			difyResponse.Answer = "<think>"
			common.SysLog("[Dify] 替换为思考开始标记")
		} else if difyResponse.Answer == "</details>" {
			difyResponse.Answer = "</think>"
			common.SysLog("[Dify] 替换为思考结束标记")
		}

		choice.Delta.SetContentString(difyResponse.Answer)
	}
	response.Choices = append(response.Choices, choice)
	common.SysLog(fmt.Sprintf("[Dify] 返回OpenAI格式响应, 选项数: %d", len(response.Choices)))
	return &response
}

func difyStreamHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*dto.OpenAIErrorWithStatusCode, *dto.Usage) {
	common.SysLog(fmt.Sprintf("[Dify] 开始处理流式响应, 状态码: %d", resp.StatusCode))
	var responseText string
	usage := &dto.Usage{}
	var nodeToken int
	helper.SetEventStreamHeaders(c)
	streamCount := 0

	helper.StreamScannerHandler(c, resp, info, func(data string) bool {
		streamCount++
		if streamCount <= 5 || streamCount%50 == 0 {
			common.SysLog(fmt.Sprintf("[Dify] 处理第%d个数据块, 长度: %d", streamCount, len(data)))
		}

		var difyResponse DifyChunkChatCompletionResponse
		err := json.Unmarshal([]byte(data), &difyResponse)
		if err != nil {
			common.SysError("[Dify] error unmarshalling stream response: " + err.Error())
			return true
		}

		var openaiResponse dto.ChatCompletionsStreamResponse
		if difyResponse.Event == "message_end" {
			common.SysLog(fmt.Sprintf("[Dify] 消息结束事件, 使用量: %+v", difyResponse.MetaData.Usage))
			usage = &difyResponse.MetaData.Usage
			return false
		} else if difyResponse.Event == "error" {
			common.SysLog("[Dify] 错误事件")
			return false
		} else {
			openaiResponse = *streamResponseDify2OpenAI(difyResponse)
			if len(openaiResponse.Choices) != 0 {
				contentStr := openaiResponse.Choices[0].Delta.GetContentString()
				responseText += contentStr

				if streamCount <= 3 || streamCount%100 == 0 {
					displayContent := contentStr
					if len(displayContent) > 30 {
						displayContent = displayContent[:30] + "..."
					}
					common.SysLog(fmt.Sprintf("[Dify] 累计响应长度: %d, 当前块: %s",
						len(responseText), displayContent))
				}

				if openaiResponse.Choices[0].Delta.ReasoningContent != nil {
					nodeToken += 1
					common.SysLog("[Dify] 增加节点token")
				}
			}
		}
		err = helper.ObjectData(c, openaiResponse)
		if err != nil {
			common.SysError("[Dify] " + err.Error())
		}
		return true
	})
	common.SysLog(fmt.Sprintf("[Dify] 流处理结束, 总块数: %d, 总响应长度: %d", streamCount, len(responseText)))
	helper.Done(c)
	err := resp.Body.Close()
	if err != nil {
		common.SysError("[Dify] close_response_body_failed: " + err.Error())
	}
	if usage.TotalTokens == 0 {
		usage.PromptTokens = info.PromptTokens
		usage.CompletionTokens, _ = service.CountTextToken("gpt-3.5-turbo", responseText)
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		common.SysLog(fmt.Sprintf("[Dify] 计算token使用量: 提示: %d, 补全: %d, 总计: %d",
			usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens))
	}
	usage.CompletionTokens += nodeToken
	common.SysLog(fmt.Sprintf("[Dify] 最终token使用量: %+v", usage))
	return nil, usage
}

func difyHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*dto.OpenAIErrorWithStatusCode, *dto.Usage) {
	common.SysLog(fmt.Sprintf("[Dify] 开始处理非流式响应, 状态码: %d", resp.StatusCode))
	var difyResponse DifyChatCompletionResponse
	responseBody, err := io.ReadAll(resp.Body)

	if err != nil {
		common.SysError("[Dify] read_response_body_failed: " + err.Error())
		return service.OpenAIErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError), nil
	}
	err = resp.Body.Close()
	if err != nil {
		common.SysError("[Dify] close_response_body_failed: " + err.Error())
		return service.OpenAIErrorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), nil
	}

	// Log the raw response body before attempting to unmarshal
	responseLength := len(responseBody)
	displayResponse := string(responseBody)
	if responseLength > 200 {
		displayResponse = displayResponse[:200] + "..."
	}
	common.SysLog(fmt.Sprintf("[Dify] 原始响应, 长度: %d, 内容: %s", responseLength, displayResponse))

	err = json.Unmarshal(responseBody, &difyResponse)
	if err != nil {
		common.SysError("[Dify] unmarshal_response_body_failed: " + err.Error())
		return service.OpenAIErrorWrapper(err, "unmarshal_response_body_failed", http.StatusInternalServerError), nil
	}
	common.SysLog(fmt.Sprintf("[Dify] 解析响应成功, 会话ID: %s, 使用量: %+v",
		difyResponse.ConversationId, difyResponse.MetaData.Usage))

	fullTextResponse := dto.OpenAITextResponse{
		Id:      difyResponse.ConversationId,
		Object:  "chat.completion",
		Created: common.GetTimestamp(),
		Usage:   difyResponse.MetaData.Usage,
	}
	content, _ := json.Marshal(difyResponse.Answer)
	choice := dto.OpenAITextResponseChoice{
		Index: 0,
		Message: dto.Message{
			Role:    "assistant",
			Content: content,
		},
		FinishReason: "stop",
	}
	fullTextResponse.Choices = append(fullTextResponse.Choices, choice)
	jsonResponse, err := json.Marshal(fullTextResponse)
	if err != nil {
		common.SysError("[Dify] marshal_response_body_failed: " + err.Error())
		return service.OpenAIErrorWrapper(err, "marshal_response_body_failed", http.StatusInternalServerError), nil
	}

	common.SysLog(fmt.Sprintf("[Dify] 转换为OpenAI响应格式完成, 长度: %d", len(jsonResponse)))
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, err = c.Writer.Write(jsonResponse)
	if err != nil {
		common.SysError("[Dify] 写入响应失败: " + err.Error())
	}
	common.SysLog("[Dify] 响应处理完成")
	return nil, &difyResponse.MetaData.Usage
}
