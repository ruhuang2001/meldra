package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

const defaultModel = "gpt-5.6-luna"

type ToolDefinition struct {
	Name        string
	Description string
	Parameters  map[string]any
	Function    func(json.RawMessage) (string, error)
}

var ReadFileDefinition = ToolDefinition{
	Name:        "read_file",
	Description: "Read the contents of a file at a given path. Use this when you want to see what's inside a file. Do not use this with directory names.",
	Parameters: objectSchema(map[string]any{
		"path": map[string]any{
			"type":        "string",
			"description": "The path of the file to read, relative to the working directory or absolute.",
		},
	}, []string{"path"}),
	Function: ReadFile,
}

var ListFilesDefinition = ToolDefinition{
	Name:        "list_files",
	Description: "Recursively list files and directories at a given path. Use null for path to list the current working directory.",
	Parameters: objectSchema(map[string]any{
		"path": map[string]any{
			"type":        []string{"string", "null"},
			"description": "The path to list, relative to the working directory or absolute. Use null for the current directory.",
		},
	}, []string{"path"}),
	Function: ListFiles,
}

var EditFileDefinition = ToolDefinition{
	Name: "edit_file",
	Description: `Make an exact edit to a text file by replacing old_str with new_str.
old_str and new_str must be different. old_str must occur exactly once.
To create a new file, use an empty old_str; missing parent directories will also be created.`,
	Parameters: objectSchema(map[string]any{
		"path": map[string]any{
			"type":        "string",
			"description": "The path of the file to edit or create, relative to the working directory or absolute.",
		},
		"old_str": map[string]any{
			"type":        "string",
			"description": "The exact text to replace. It must occur exactly once. Use an empty string only when creating a file.",
		},
		"new_str": map[string]any{
			"type":        "string",
			"description": "The replacement text or the complete contents of a newly created file.",
		},
	}, []string{"path", "old_str", "new_str"}),
	Function: EditFile,
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	}
}

func main() {
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		fmt.Printf("Error loading .env: %s\n", err)
		return
	}

	client := openai.NewClient()

	scanner := bufio.NewScanner(os.Stdin)
	getUserMessage := func() (string, bool) {
		if !scanner.Scan() {
			return "", false
		}
		return scanner.Text(), true
	}

	tools := []ToolDefinition{
		ReadFileDefinition,
		ListFilesDefinition,
		EditFileDefinition,
	}
	agent := NewAgent(&client, getUserMessage, tools)
	err := agent.Run(context.TODO())
	if err != nil {
		fmt.Printf("Error: %s\n", err.Error())
	}
}

func NewAgent(client *openai.Client, getUserMessage func() (string, bool), tools []ToolDefinition) *Agent {
	return &Agent{
		client:         client,
		getUserMessage: getUserMessage,
		tools:          tools,
	}
}

type Agent struct {
	client         *openai.Client
	getUserMessage func() (string, bool)
	tools          []ToolDefinition
}

func (a *Agent) Run(ctx context.Context) error {
	var previousResponseID string

	fmt.Println("Chat with Meldra (use 'ctrl-c' to quit)")

	for {
		fmt.Print("\u001b[94mYou\u001b[0m: ")
		userInput, ok := a.getUserMessage()
		if !ok {
			break
		}

		input := responses.ResponseNewParamsInputUnion{
			OfString: openai.String(userInput),
		}
		customBaseURLInput := responses.ResponseInputParam{
			responses.ResponseInputItemParamOfMessage(userInput, responses.EasyInputMessageRoleUser),
		}

		for {
			response, err := a.runInference(ctx, input, previousResponseID)
			if err != nil {
				return err
			}
			if err := validateResponse(response); err != nil {
				return err
			}
			previousResponseID = response.ID

			if text := response.OutputText(); text != "" {
				fmt.Printf("\u001b[93mMeldra\u001b[0m: %s\n", text)
			}

			toolResults := a.executeToolCalls(response.Output)
			if len(toolResults) == 0 {
				break
			}
			if usesCustomBaseURL() {
				// Some OpenAI-compatible providers do not retain function calls
				// referenced by previous_response_id. Include those calls again so
				// their corresponding outputs can be matched by call_id. Keep the
				// original user request and every tool exchange from this turn too.
				customBaseURLInput = append(customBaseURLInput, toolFollowUpInput(response.Output, toolResults)...)
				input = responses.ResponseNewParamsInputUnion{
					OfInputItemList: customBaseURLInput,
				}
				previousResponseID = ""
				continue
			}
			input = responses.ResponseNewParamsInputUnion{
				OfInputItemList: toolResults,
			}
		}
	}

	return nil
}

func (a *Agent) runInference(ctx context.Context, input responses.ResponseNewParamsInputUnion, previousResponseID string) (*responses.Response, error) {
	tools := make([]responses.ToolUnionParam, 0, len(a.tools))
	for _, tool := range a.tools {
		tools = append(tools, responses.ToolUnionParam{
			OfFunction: &responses.FunctionToolParam{
				Name:        tool.Name,
				Description: openai.String(tool.Description),
				Parameters:  tool.Parameters,
				Strict:      openai.Bool(true),
			},
		})
	}

	params := responses.ResponseNewParams{
		Model: modelName(),
		Input: input,
		Tools: tools,
	}
	if previousResponseID != "" {
		params.PreviousResponseID = openai.String(previousResponseID)
	}

	return a.client.Responses.New(ctx, params)
}

func modelName() string {
	if model := os.Getenv("OPENAI_MODEL"); model != "" {
		return model
	}
	return defaultModel
}

func usesCustomBaseURL() bool {
	return os.Getenv("OPENAI_BASE_URL") != ""
}

func validateResponse(response *responses.Response) error {
	if response.Status == responses.ResponseStatusCompleted {
		return nil
	}
	if response.Error.Message != "" {
		return fmt.Errorf("response %s: %s", response.Status, response.Error.Message)
	}
	if response.IncompleteDetails.Reason != "" {
		return fmt.Errorf("response %s: %s", response.Status, response.IncompleteDetails.Reason)
	}
	return fmt.Errorf("response ended with status %q", response.Status)
}

func (a *Agent) executeToolCalls(output []responses.ResponseOutputItemUnion) responses.ResponseInputParam {
	var results responses.ResponseInputParam
	for _, item := range output {
		if item.Type != "function_call" {
			continue
		}

		call := item.AsFunctionCall()
		fmt.Printf("\u001b[92mtool\u001b[0m: %s\n", call.Name)

		result, err := a.executeTool(call.Name, json.RawMessage(call.Arguments))
		if err != nil {
			result = "Error: " + err.Error()
		}
		results = append(results, responses.ResponseInputItemParamOfFunctionCallOutput(call.CallID, result))
	}
	return results
}

func toolFollowUpInput(output []responses.ResponseOutputItemUnion, toolResults responses.ResponseInputParam) responses.ResponseInputParam {
	input := make(responses.ResponseInputParam, 0, len(output)+len(toolResults))
	for _, item := range output {
		if item.Type != "function_call" {
			continue
		}

		call := item.AsFunctionCall()
		input = append(input, responses.ResponseInputItemParamOfFunctionCall(call.Arguments, call.CallID, call.Name))
	}
	return append(input, toolResults...)
}

func (a *Agent) executeTool(name string, input json.RawMessage) (string, error) {
	for _, tool := range a.tools {
		if tool.Name == name {
			return tool.Function(input)
		}
	}
	return "", fmt.Errorf("tool %q not found", name)
}

type ReadFileInput struct {
	Path string `json:"path"`
}

func ReadFile(input json.RawMessage) (string, error) {
	var params ReadFileInput
	if err := decodeToolInput(input, &params, "path"); err != nil {
		return "", fmt.Errorf("decode read_file input: %w", err)
	}
	if params.Path == "" {
		return "", fmt.Errorf("path must not be empty")
	}

	content, err := os.ReadFile(params.Path)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

type ListFilesInput struct {
	Path string `json:"path"`
}

func ListFiles(input json.RawMessage) (string, error) {
	var params ListFilesInput
	if err := decodeToolInput(input, &params); err != nil {
		return "", fmt.Errorf("decode list_files input: %w", err)
	}

	dir := params.Path
	if dir == "" {
		dir = "."
	}

	files := make([]string, 0)
	if err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}
		if info.IsDir() {
			relPath += "/"
		}
		files = append(files, relPath)
		return nil
	}); err != nil {
		return "", err
	}

	result, err := json.Marshal(files)
	if err != nil {
		return "", err
	}
	return string(result), nil
}

type EditFileInput struct {
	Path   string `json:"path"`
	OldStr string `json:"old_str"`
	NewStr string `json:"new_str"`
}

func EditFile(input json.RawMessage) (string, error) {
	var params EditFileInput
	if err := decodeToolInput(input, &params, "path", "old_str", "new_str"); err != nil {
		return "", fmt.Errorf("decode edit_file input: %w", err)
	}
	if params.Path == "" || params.OldStr == params.NewStr {
		return "", fmt.Errorf("invalid input parameters")
	}

	content, err := os.ReadFile(params.Path)
	if err != nil {
		if os.IsNotExist(err) && params.OldStr == "" {
			return createNewFile(params.Path, params.NewStr)
		}
		return "", err
	}
	if params.OldStr == "" {
		return "", fmt.Errorf("old_str may be empty only when creating a new file")
	}

	oldContent := string(content)
	matches := strings.Count(oldContent, params.OldStr)
	if matches == 0 {
		return "", fmt.Errorf("old_str not found in file")
	}
	if matches > 1 {
		return "", fmt.Errorf("old_str occurs %d times in file; it must match exactly once", matches)
	}

	info, err := os.Stat(params.Path)
	if err != nil {
		return "", err
	}
	newContent := strings.Replace(oldContent, params.OldStr, params.NewStr, 1)
	if err := os.WriteFile(params.Path, []byte(newContent), info.Mode().Perm()); err != nil {
		return "", err
	}
	return "OK", nil
}

func decodeToolInput(input json.RawMessage, target any, required ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(input, &fields); err != nil {
		return err
	}
	for _, name := range required {
		if _, ok := fields[name]; !ok {
			return fmt.Errorf("missing required parameter %q", name)
		}
	}

	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func createNewFile(filePath, content string) (string, error) {
	dir := filepath.Dir(filePath)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("create parent directories: %w", err)
		}
	}
	file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", fmt.Errorf("create file: %w", err)
	}
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		_ = os.Remove(filePath)
		return "", fmt.Errorf("write new file: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close new file: %w", err)
	}
	return fmt.Sprintf("Successfully created file %s", filePath), nil
}
