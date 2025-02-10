package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/sashabaranov/go-openai"
	lua "github.com/yuin/gopher-lua"
)

// export API_KEY="" # get from deepinfra
// export MODEL="meta-llama/Llama-3.3-70B-Instruct-Turbo"
// export BASE_URL="https://api.deepinfra.com/v1/openai"
// export PORT=8080

var LuaPrompt = `If you are doing math, use a lua sandbox, which can be used accessed by writing lua code in bewteen two lua XML blocks. The code will directly be loaded into a lua sandbox. Here is an example of running a math problem in a sandbox:
user: What is 8+8?
assistant: <lua>
print(8+8)
</lua>
tool: 16
Only use Lua when performing calculations. When using lua, make sure to enclose the lua with lua XML. For non-mathematical queries, respond normally without Lua code. Always be concise.`

func main() {
	app := InitializeApp()
	// Serve application
	s := &http.Server{
		Addr:    ":" + os.Getenv("PORT"),
		Handler: app.Router,
	}
	log.Fatal(s.ListenAndServe())
}

/*
*****************************************************************************

# Chat functions

*****************************************************************************
*/

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024 * 1024 * 16,
	WriteBufferSize: 1024 * 1024 * 16,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

func chatHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	defer conn.Close()

	apiKey := os.Getenv("API_KEY")
	baseUrl := os.Getenv("BASE_URL")
	model := os.Getenv("MODEL")
	config := openai.DefaultConfig(apiKey)
	if baseUrl != "" {
		config.BaseURL = baseUrl
	}
	client := openai.NewClientWithConfig(config)

	for {
		messageType, p, err := conn.ReadMessage()
		if err != nil {
			log.Println(err)
			return
		}
		msg := string(p)

		// Parse existing conversation or create new one
		var messages []openai.ChatCompletionMessage
		if len(msg) > 14 && msg[0:15] == "<|begin_of_text" {
			// Use existing conversation context
			parsedMsgs := parseToMessages(msg)
			for _, m := range parsedMsgs {
				role := m.Role
				// Map roles to OpenAI chat roles
				switch role {
				case "system":
					role = "system"
				case "user":
					role = "user"
				case "assistant":
					role = "assistant"
				}
				messages = append(messages, openai.ChatCompletionMessage{
					Role:    role,
					Content: m.Content,
				})
			}
		} else {
			// Create new conversation
			messages = []openai.ChatCompletionMessage{
				{
					Role:    "system",
					Content: LuaPrompt,
				},
				{
					Role:    "user",
					Content: msg,
				},
			}
		}

		stream, err := client.CreateChatCompletionStream(
			context.Background(),
			openai.ChatCompletionRequest{
				Model:    model,
				Messages: messages,
				Stream:   true,
			},
		)

		if err != nil {
			fmt.Printf("ChatCompletionStream error: %v\n", err)
			return
		}

		var luaCode strings.Builder
		// Construct and send the conversation context
		contextMsg := constructConversationContext(messages)
		_ = conn.WriteMessage(messageType, []byte(contextMsg))

		var insideLua bool = false
		for {
			var response openai.ChatCompletionStreamResponse
			response, err = stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}

			if err != nil {
				fmt.Printf("\nStream error: %v\n", err)
				break
			}

			if len(response.Choices) > 0 {
				token := response.Choices[0].Delta.Content
				if strings.Contains(luaCode.String(), "<lua>") {
					insideLua = true
				}

				luaCode.WriteString(token)
				_ = conn.WriteMessage(messageType, []byte(token))
			}
		}

		stream.Close()

		// Handle Lua execution if present
		input := luaCode.String()
		var luaRawCode string
		if insideLua {
			luaPrefix := "<lua>"
			codeSuffix := "</lua>"
			luaStartIndex := strings.Index(input, luaPrefix)
			if luaStartIndex != -1 {
				luaEndIndex := strings.Index(input[luaStartIndex+len(luaPrefix):], codeSuffix)
				if luaEndIndex != -1 {
					luaRawCode = input[luaStartIndex+len(luaPrefix) : luaStartIndex+len(luaPrefix)+luaEndIndex]
				}
			}

			output, err := ExecuteLua(luaRawCode)
			toolHeader := "\n<|eot_id|>\n<|start_header_id|>assistant<|end_header_id|>\ntool:\n"
			_ = conn.WriteMessage(messageType, []byte(toolHeader))

			if err != nil {
				errorMsg := fmt.Sprintf("Got error: %s", err.Error())
				_ = conn.WriteMessage(messageType, []byte(errorMsg))
			} else {
				_ = conn.WriteMessage(messageType, []byte(output))
			}
		}

		userHeader := "\n<|eot_id|>\n<|start_header_id|>user<|end_header_id|>\n"
		_ = conn.WriteMessage(messageType, []byte(userHeader))
	}
}

// Helper function to construct conversation context in the expected format
func constructConversationContext(messages []openai.ChatCompletionMessage) string {
	var result strings.Builder
	result.WriteString("<|begin_of_text|>")

	for i, msg := range messages {
		if i > 0 {
			result.WriteString("\n<|eot_id|>\n")
		}

		role := msg.Role
		// Map OpenAI roles back to our format
		switch role {
		case "system":
			result.WriteString("<|start_header_id|>system<|end_header_id|>\n")
		case "user":
			result.WriteString("<|start_header_id|>user<|end_header_id|>\n")
		case "assistant":
			result.WriteString("<|start_header_id|>assistant<|end_header_id|>\n")
		}

		result.WriteString(msg.Content)
	}

	result.WriteString("\n<|eot_id|>\n<|start_header_id|>assistant<|end_header_id|>\n")
	return result.String()
}

// customPrint is a function that mimics Lua's print function but writes to an io.Writer.
func customPrint(writer io.Writer) func(L *lua.LState) int {
	return func(L *lua.LState) int {
		top := L.GetTop()
		for i := 1; i <= top; i++ {
			str := L.ToString(i) // Convert each argument to a string as Lua's print does.
			if i > 1 {
				io.WriteString(writer, "\t")
			}
			io.WriteString(writer, str)
		}
		return 0 // Number of results.
	}
}

func ExecuteLua(code string) (string, error) {
	L := lua.NewState()
	defer L.Close()

	// Add stdout
	var buffer strings.Builder
	L.SetGlobal("print", L.NewFunction(customPrint(&buffer)))

	// Execute the Lua script
	if err := L.DoString(code); err != nil {
		return "", err
	}

	return buffer.String(), nil
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func parseToMessages(input string) []Message {
	var messages []Message

	// Split on message boundaries
	parts := strings.Split(input, "<|eot_id|>")

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// Find header
		headerStart := strings.Index(part, "<|start_header_id|>")
		headerEnd := strings.Index(part, "<|end_header_id|>")

		if headerStart == -1 || headerEnd == -1 {
			continue
		}

		// Extract role and content
		role := strings.TrimSpace(part[headerStart+len("<|start_header_id|>") : headerEnd])
		content := strings.TrimSpace(part[headerEnd+len("<|end_header_id|>"):])

		// Skip empty content
		if content == "" {
			continue
		}

		// Handle special cases
		switch role {
		case "system":
			// Remove begin_of_text marker if present
			content = strings.TrimPrefix(content, "<|begin_of_text|>")
		}

		content = strings.TrimSpace(content)

		// Only add message if we have both role and content
		if role != "" && content != "" {
			messages = append(messages, Message{
				Role:    role,
				Content: content,
			})
		}
	}

	return messages
}

/*
*****************************************************************************

# Web server

*****************************************************************************
*/

//go:embed index.html
var indexHtml string

func indexHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	_, err := fmt.Fprint(w, indexHtml)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// App implements the app
type App struct {
	Router *http.ServeMux
	Logger *slog.Logger
}

// initializeApp starts here
func InitializeApp() App {
	var app App
	app.Router = http.NewServeMux()
	app.Logger = slog.Default()
	app.Router.HandleFunc("/", indexHandler)
	app.Router.HandleFunc("/chat", chatHandler)
	return app
}
