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
// export MODEL="microsoft/WizardLM-2-8x22B"
// export BASE_URL="https://api.deepinfra.com/v1/openai"
// export PORT=8080

var LuaPrompt = "Please respond ONLY with valid lua. The code will directly be loaded into a lua sandbox. If ANY math is used, write lua to do the math, do not attempt it yourself. Print a statement back to the user that answers their query. Be as concise as possible."

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
		// A long message might indicate we need to add to the prompt
		var prompt string
		systemStart := "<|im_start|>system\n"
		userRequest := "<|im_end|>\n<|im_start|>user\n"
		assistantCompletion := "\n<|im_end|>\n<|im_start|>assistant\n```lua\n"
		toolCompletion := "<|im_end|>\n<|im_start|>tool\nTool Output:\n"
		if len(msg) > 14 {
			if msg[0:12] == "<|im_start|>" {
				prompt = msg + assistantCompletion
			}
		}
		if prompt == "" {
			prompt = systemStart + LuaPrompt + userRequest + msg + assistantCompletion
		}

		stream, err := client.CreateCompletionStream(
			context.Background(),
			openai.CompletionRequest{
				Model:  model,
				Prompt: prompt,
				Stream: true,
				Stop:   []string{"```"},
			},
		)
		if err != nil {
			fmt.Printf("CompletionStream error: %v\n", err)
			return
		}

		var luaCode strings.Builder
		luaCode.WriteString("```lua\n")
		_ = conn.WriteMessage(messageType, []byte(prompt))
		var promptTokens int
		var completionTokens int
		var totalTokens int
		for {
			var response openai.CompletionResponse
			response, err = stream.Recv()
			if errors.Is(err, io.EOF) {
				finalLine := ""
				for !strings.HasSuffix(luaCode.String()+finalLine, "```") {
					finalLine = finalLine + "`"
				}
				finalLine = finalLine + "\n"
				luaCode.WriteString(finalLine)
				_ = conn.WriteMessage(messageType, []byte(finalLine))
				totalTokens = promptTokens + completionTokens
				break
			}

			if err != nil {
				fmt.Printf("\nStream error: %v\n", err)
				break
			}
			token := response.Choices[0].Text
			luaCode.WriteString(token)
			_ = conn.WriteMessage(messageType, []byte(token))

			promptTokens = response.Usage.PromptTokens
			completionTokens = completionTokens + response.Usage.CompletionTokens
		}
		fmt.Printf("PromptTokens=%d, CompletionTokens=%d, TotalTokens=%d\n", promptTokens, completionTokens, totalTokens)
		stream.Close()
		// Parse out the lua
		input := luaCode.String()
		prompt = prompt + input
		var luaRawCode string
		// Check for ```lua ... ```
		luaPrefix := "```lua"
		codeSuffix := "```"
		luaStartIndex := strings.Index(input, luaPrefix)
		if luaStartIndex != -1 {
			luaEndIndex := strings.Index(input[luaStartIndex+len(luaPrefix):], codeSuffix)
			if luaEndIndex != -1 {
				luaRawCode = input[luaStartIndex+len(luaPrefix) : luaStartIndex+len(luaPrefix)+luaEndIndex]
			}
		}
		// Execute the lua
		output, err := ExecuteLua(luaRawCode)
		_ = conn.WriteMessage(messageType, []byte(toolCompletion))
		prompt = prompt + toolCompletion
		if err != nil {
			errorMsg := fmt.Sprintf("Got error: %s", err.Error())
			prompt = prompt + errorMsg
			_ = conn.WriteMessage(messageType, []byte(errorMsg))
		} else {
			prompt = prompt + output
			_ = conn.WriteMessage(messageType, []byte(output))
		}

		_ = conn.WriteMessage(messageType, []byte(userRequest))
	}
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
		io.WriteString(writer, "\n")
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
