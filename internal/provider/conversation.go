package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
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
	// A JSON array's size is the sum of its encoded items, separators and
	// brackets. Measure each item once; repeatedly marshaling the entire array
	// after replacing each old output makes a long conversation quadratic.
	size := 2 + max(0, len(input)-1)
	if input == nil {
		size = len("null")
	}
	sizes := make([]int, len(input))
	for index, item := range input {
		encoded, err := json.Marshal(item)
		if err != nil {
			return nil, 0, false, fmt.Errorf("measure custom-provider context: %w", err)
		}
		sizes[index] = len(encoded)
		size += len(encoded)
	}
	if size <= limit {
		return input, size, false, nil
	}
	bounded := slices.Clone(input)
	compacted := false
	for index, item := range bounded {
		if item.OfFunctionCallOutput == nil {
			continue
		}
		replacement, err := compactFunctionOutput(item)
		if err != nil {
			return nil, 0, compacted, fmt.Errorf("compact custom-provider context: %w", err)
		}
		encoded, err := json.Marshal(replacement)
		if err != nil {
			return nil, 0, compacted, fmt.Errorf("measure compacted custom-provider context: %w", err)
		}
		if len(encoded) >= sizes[index] {
			continue
		}
		bounded[index] = replacement
		compacted = true
		size += len(encoded) - sizes[index]
		if size <= limit {
			return bounded, size, compacted, nil
		}
	}
	return nil, size, compacted, fmt.Errorf("custom-provider context exceeds %d byte limit", limit)
}

func compactFunctionOutput(item responses.ResponseInputItemUnionParam) (responses.ResponseInputItemUnionParam, error) {
	// Copy the output before changing its body: IDs, status and caller metadata
	// are part of the replay protocol, and callers may still retain the input.
	output := *item.OfFunctionCallOutput
	output.Output = responses.ResponseInputItemFunctionCallOutputOutputUnionParam{OfString: openai.String(compactedToolOutput)}
	replacement := item
	replacement.OfFunctionCallOutput = &output
	_, itemOverride := item.Overrides()
	_, outputOverride := item.OfFunctionCallOutput.Overrides()
	if !itemOverride && !outputOverride && len(item.ExtraFields()) == 0 && len(output.ExtraFields()) == 0 {
		return replacement, nil
	}

	// SDK overrides/extra fields can take precedence over typed fields. Retain
	// their actual wire representation while replacing only the output body.
	encoded, err := json.Marshal(item)
	if err != nil {
		return replacement, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return replacement, err
	}
	if fields == nil {
		return replacement, errors.New("function output override is not an object")
	}
	fields["output"], _ = json.Marshal(compactedToolOutput)
	encoded, err = json.Marshal(fields)
	if err != nil {
		return replacement, err
	}
	// Override the copied output rather than the union: the SDK gives a present
	// union variant precedence over the union's own override. Keeping the typed
	// variant also lets later checks identify an already compacted output.
	param.SetJSON(encoded, &output)
	return replacement, nil
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
		seen := map[string]bool{}
		for _, item := range response.Output {
			if item.Type != "function_call" {
				continue
			}
			if item.CallID == "" || seen[item.CallID] {
				return errors.New("response contains missing or duplicate tool call identity")
			}
			seen[item.CallID] = true
		}
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
