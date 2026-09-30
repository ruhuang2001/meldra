package provider

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

func toolFollowUpInput(output []responses.ResponseOutputItemUnion, toolResults responses.ResponseInputParam) (responses.ResponseInputParam, error) {
	input := make(responses.ResponseInputParam, 0, len(output)+len(toolResults))
	for _, item := range output {
		switch item.Type {
		case "message":
			message := item.AsMessage().ToParam()
			input = append(input, responses.ResponseInputItemUnionParam{OfOutputMessage: &message})
		case "reasoning":
			reasoning := item.AsReasoning().ToParam()
			input = append(input, responses.ResponseInputItemUnionParam{OfReasoning: &reasoning})
		case "function_call":
			call := item.AsFunctionCall().ToParam()
			input = append(input, responses.ResponseInputItemUnionParam{OfFunctionCall: &call})
		default:
			return nil, fmt.Errorf("cannot replay unsupported response output type %q for custom base URL", item.Type)
		}
	}
	return append(input, toolResults...), nil
}

const compactedToolOutput = "[older tool output omitted to fit the custom-provider context budget]"

func boundCustomTurnInput(input responses.ResponseInputParam, limit int) (responses.ResponseInputParam, int, bool, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, 0, false, fmt.Errorf("measure custom-provider context: %w", err)
	}
	if len(encoded) <= limit {
		return input, len(encoded), false, nil
	}
	bounded := append(responses.ResponseInputParam(nil), input...)
	compacted := false
	for index, item := range bounded {
		if item.OfFunctionCallOutput == nil {
			continue
		}
		bounded[index] = responses.ResponseInputItemParamOfFunctionCallOutput(item.OfFunctionCallOutput.CallID, compactedToolOutput)
		compacted = true
		encoded, err = json.Marshal(bounded)
		if err != nil {
			return nil, 0, compacted, fmt.Errorf("measure compacted custom-provider context: %w", err)
		}
		if len(encoded) <= limit {
			return bounded, len(encoded), compacted, nil
		}
	}
	return nil, len(encoded), compacted, fmt.Errorf("custom-provider context exceeds %d byte limit", limit)
}

// Item carries opaque provider continuation data. The runner may retain and
// concatenate items, but only the provider interprets their wire representation.
type Item struct {
	value responses.ResponseInputItemUnionParam
}
type Items []Item

func (i Item) MarshalJSON() ([]byte, error) { return json.Marshal(i.value) }

type Input struct {
	value responses.ResponseNewParamsInputUnion
}

func UserInput(text string) Input {
	return Input{value: responses.ResponseNewParamsInputUnion{OfString: openai.String(text)}}
}
func UserItems(text string) Items {
	return Items{{value: responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleUser)}}
}
func ToolOutput(callID, output string) Item {
	return Item{value: responses.ResponseInputItemParamOfFunctionCallOutput(callID, output)}
}
func ItemsInput(items Items) Input {
	return Input{value: responses.ResponseNewParamsInputUnion{OfInputItemList: wireItems(items)}}
}
func wireItems(items Items) responses.ResponseInputParam {
	result := make(responses.ResponseInputParam, len(items))
	for i, item := range items {
		result[i] = item.value
	}
	return result
}
func domainItems(items responses.ResponseInputParam) Items {
	result := make(Items, len(items))
	for i, item := range items {
		result[i] = Item{value: item}
	}
	return result
}
func BoundInput(items Items, limit int) (Items, int, bool, error) {
	bounded, size, compacted, err := boundCustomTurnInput(wireItems(items), limit)
	return domainItems(bounded), size, compacted, err
}
func FollowUp(output []OutputItem, results Items) (Items, error) {
	items := make([]responses.ResponseOutputItemUnion, len(output))
	for i, item := range output {
		items[i] = item.value
	}
	followUp, err := toolFollowUpInput(items, wireItems(results))
	return domainItems(followUp), err
}

// OutputItem exposes only the fields the tool dispatcher needs. All provider
// metadata needed for replay is retained privately without leaking SDK types.
type OutputItem struct {
	Type      string
	CallID    string
	Name      string
	Arguments string
	value     responses.ResponseOutputItemUnion
}

func outputItem(item responses.ResponseOutputItemUnion) OutputItem {
	call := item.AsFunctionCall()
	return OutputItem{Type: item.Type, CallID: call.CallID, Name: call.Name, Arguments: call.Arguments, value: item}
}
func (item *OutputItem) UnmarshalJSON(data []byte) error {
	var value responses.ResponseOutputItemUnion
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*item = outputItem(value)
	return nil
}

// Response is the completed model result consumed by the application.
type Response struct {
	ID               string
	Status           string
	Text             string
	ErrorMessage     string
	IncompleteReason string
	Output           []OutputItem
	InputTokens      int64
	OutputTokens     int64
}

func (r *Response) OutputText() string {
	if r == nil {
		return ""
	}
	return r.Text
}
func domainResponse(response *responses.Response) *Response {
	if response == nil {
		return nil
	}
	result := &Response{ID: response.ID, Status: string(response.Status), Text: OutputText(response), ErrorMessage: response.Error.Message,
		IncompleteReason: response.IncompleteDetails.Reason, InputTokens: response.Usage.InputTokens, OutputTokens: response.Usage.OutputTokens}
	for _, item := range response.Output {
		result.Output = append(result.Output, outputItem(item))
	}
	return result
}
func Validate(response *Response) error {
	if response == nil {
		return errors.New("response stream completed without a response")
	}
	if response.Status == "completed" {
		return nil
	}
	if response.ErrorMessage != "" {
		return fmt.Errorf("response %s: %s", response.Status, response.ErrorMessage)
	}
	if response.IncompleteReason != "" {
		return fmt.Errorf("response %s: %s", response.Status, response.IncompleteReason)
	}
	return fmt.Errorf("response ended with status %q", response.Status)
}
