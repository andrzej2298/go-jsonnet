package jsonnet

import (
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"strings"

	"github.com/google/go-jsonnet/ast"
	"github.com/wasmerio/wasmer-go/wasmer"
	"go.mongodb.org/mongo-driver/bson"
)

func (wasmFunction *wasmFunction) callFunction(functionName string, arguments []byte) (bson.Raw, error) {
	return callFunction(wasmFunction.wasmerInstance, functionName, arguments)
}

func callFunction(wasmerInstance *wasmer.Instance, functionName string, argumentBuffer []byte) (bson.Raw, error) {
	allocate, err := wasmerInstance.Exports.GetFunction("__jsonnet_internal_allocate")
	if err != nil {
		return nil, err
	}
	deallocate, err := wasmerInstance.Exports.GetFunction("__jsonnet_internal_deallocate")
	if err != nil {
		return nil, err
	}
	memoryExport, err := wasmerInstance.Exports.GetMemory("memory")
	if err != nil {
		return nil, err
	}
	function, err := wasmerInstance.Exports.GetFunction(functionName)
	if err != nil {
		return nil, err
	}
	var bsonResult interface{}

	if len(argumentBuffer) > 0 {
		argumentsLen := len(argumentBuffer)

		// prepare input
		allocateResult, err := allocate(argumentsLen)
		if err != nil {
			return nil, err
		}
		inputPointer := allocateResult.(int32)
		inputMemoryChunk := memoryExport.Data()[inputPointer:]

		for i := 0; i < argumentsLen; i++ {
			inputMemoryChunk[i] = argumentBuffer[i]
		}
		// get output
		bsonResult, err = function(inputPointer)
		if err != nil {
			return nil, err
		}
	} else {
		bsonResult, err = function()
		if err != nil {
			return nil, err
		}
	}

	// parse output
	outputPointer := bsonResult.(int32)
	outputMemoryChunk := memoryExport.Data()[outputPointer:]
	outputSize := binary.LittleEndian.Uint32(outputMemoryChunk)

	// TODO: copy?
	var raw bson.Raw = outputMemoryChunk

	deallocate(outputPointer, outputSize)
	return raw, nil
}

type wasmFunction struct {
	functionName   string
	wasmerInstance *wasmer.Instance
	params         ast.Identifiers
}

func makeWasmerInstance(filePath string) (*wasmer.Instance, []string, error) {
	// Create an engine. It's responsible for driving the compilation and the
	// execution of a WebAssembly module.
	engine := wasmer.NewEngine()

	// Create a store, that holds the engine.
	store := wasmer.NewStore(engine)

	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, nil, err
	}

	// Create a new module from the file with the lib.
	module, err := wasmer.NewModule(
		store,
		data,
	)
	if err != nil {
		return nil, nil, err
	}

	// wasiEnv, err := wasmer.NewWasiStateBuilder("wasi-test-program").Environment("RUST_BACKTRACE", "full").CaptureStdout().CaptureStderr().Finalize()
	wasiEnv, err := wasmer.NewWasiStateBuilder("wasi-test-program").Environment("RUST_BACKTRACE", "full").Finalize()
	if err != nil {
		return nil, nil, err
	}
	importObject, err := wasiEnv.GenerateImportObject(store, module)
	if err != nil {
		return nil, nil, err
	}
	instance, err := wasmer.NewInstance(module, importObject)
	if err != nil {
		return nil, nil, err
	}

	var functionNames []string

	// lookup all function names in the module (only exported functions, ie. those that have a "__jsonnet_export_" prefix)
	for _, v := range module.Exports() {
		if v.Type().Kind().String() == "func" && strings.HasPrefix(v.Name(), "__jsonnet_export_") {
			functionNames = append(functionNames, strings.TrimPrefix(v.Name(), "__jsonnet_export_"))
		}
	}

	return instance, functionNames, nil
}

func makeWASMFunction(functionName string, wasmerInstance *wasmer.Instance) (*wasmFunction, error) {
	metadataFunction := fmt.Sprintf("__jsonnet_internal_meta_%v", functionName)
	result, err := callFunction(wasmerInstance, metadataFunction, []byte{})
	if err != nil {
		return nil, err
	}
	var paramNames []ast.Identifier
	err = result.Lookup("").Unmarshal(&paramNames)
	if err != nil {
		return nil, err
	}

	return &wasmFunction{
		functionName:   functionName,
		wasmerInstance: wasmerInstance,
		params:         paramNames,
	}, nil
}

func (wasmFunction *wasmFunction) evalCall(arguments callArguments, i *interpreter) (value, error) {
	flatArgs := flattenArgs(arguments, wasmFunction.parameters(), []value{})
	var bsonResult bson.Raw
	exportedFunctionName := "__jsonnet_export_" + wasmFunction.functionName
	if len(flatArgs) > 0 {
		wasmArgs := make(map[string]interface{})
		for index, arg := range flatArgs {
			v, err := i.evaluatePV(arg)
			if err != nil {
				return nil, err
			}
			json, err := i.manifestJSON(v)
			if err != nil {
				return nil, err
			}
			wasmArgs[string(wasmFunction.params[index])] = json
		}
		marshalledArgs, err := bson.Marshal(wasmArgs)
		if err != nil {
			return nil, err
		}
		bsonResult, err = wasmFunction.callFunction(exportedFunctionName, marshalledArgs)
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		bsonResult, err = wasmFunction.callFunction(exportedFunctionName, []byte{})
		if err != nil {
			return nil, err
		}
	}

	return bsonToValue(bsonResult.Lookup(""))
}

type wasmError struct {
	field string
}

func (e *wasmError) Error() string {
	return e.field
}

func bsonToValue(bson bson.RawValue) (value, error) {
	switch bson.Type {
	case '\x01':
		return makeValueNumber(bson.Double()), nil
	case '\x02':
		return makeValueString(bson.StringValue()), nil
	case '\x03':
		resultFields := make(simpleObjectFieldMap)
		fields, err := bson.Document().Elements()
		if err != nil {
			return nil, err
		}
		for _, field := range fields {
			fieldValue, err := bsonToValue(field.Value())
			if err != nil {
				return nil, err
			}
			resultFields[field.Key()] = simpleObjectField{hide: ast.ObjectFieldVisible, field: &readyValue{fieldValue}}
		}
		var asserts []unboundField
		var locals []objectLocal
		var bindingFrame = make(bindingFrame)
		return makeValueSimpleObject(bindingFrame, resultFields, asserts, locals), nil
	case '\x04':
		var resultElements []*cachedThunk
		elements, err := bson.Array().Elements()
		if err != nil {
			return nil, err
		}
		for _, element := range elements {
			elementValue, err := bsonToValue(element.Value())
			if err != nil {
				return nil, err
			}
			resultElements = append(resultElements, readyThunk(elementValue))
		}
		return makeValueArray(resultElements), nil
	case '\x08':
		return makeValueBoolean(bson.Boolean()), nil
	case '\x0A':
		return makeValueNull(), nil
	case '\x10':
		return makeValueNumber(float64(bson.Int32())), nil
	case '\x12':
		return int64ToValue(bson.Int64()), nil
	default:
		return nil, &wasmError{field: fmt.Sprintf("couldn't serialize field of type %v", bson.Type)}
	}
}

func (wasmFunction *wasmFunction) parameters() []namedParameter {
	ret := make([]namedParameter, len(wasmFunction.params))
	for i := range ret {
		ret[i].name = wasmFunction.params[i]
	}
	return ret
}
