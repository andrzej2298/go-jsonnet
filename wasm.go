package jsonnet

import (
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"path"
	"strings"

	"github.com/google/go-jsonnet/ast"
	"github.com/wasmerio/wasmer-go/wasmer"
	"go.mongodb.org/mongo-driver/bson"
)

func (wasmFunction *wasmFunction) callFunction(functionName string, arguments []byte) bson.Raw {
	return callFunction(wasmFunction.wasmerInstance, functionName, arguments)
}

func callFunction(wasmerInstance *wasmer.Instance, functionName string, arguments []byte) bson.Raw {
	var rawArguments bson.Raw = arguments
	fmt.Println(rawArguments)
	allocate, _ := wasmerInstance.Exports.GetFunction("__jsonnet_internal_allocate")
	deallocate, _ := wasmerInstance.Exports.GetFunction("__jsonnet_internal_deallocate")
	memoryExport, _ := wasmerInstance.Exports.GetMemory("memory")
	function, _ := wasmerInstance.Exports.GetFunction(functionName)
	var bsonResult interface{}

	if len(arguments) > 0 {
		argumentsLen := len(arguments)

		// prepare input
		allocateResult, _ := allocate(argumentsLen)
		inputPointer := allocateResult.(int32)
		inputMemoryChunk := memoryExport.Data()[inputPointer:]

		for i := 0; i < argumentsLen; i++ {
			inputMemoryChunk[i] = arguments[i]
		}
		// get output
		bsonResult, _ = function(inputPointer)
		deallocate(inputPointer, argumentsLen)
	} else {
		bsonResult, _ = function()
	}

	// parse output
	outputPointer := bsonResult.(int32)
	outputMemoryChunk := memoryExport.Data()[outputPointer:]
	outputSize := binary.LittleEndian.Uint32(outputMemoryChunk)

	var raw bson.Raw = outputMemoryChunk

	deallocate(outputPointer, outputSize)
	return raw
}

type wasmFunction struct {
	functionName   string
	wasmerInstance *wasmer.Instance
	params         ast.Identifiers
}

func makeWasmerInstance(fileDir, fileName string) (*wasmer.Instance, []string) {
	// Create an engine. It's responsible for driving the compilation and the
	// execution of a WebAssembly module.
	engine := wasmer.NewEngine()

	// Create a store, that holds the engine.
	store := wasmer.NewStore(engine)

	dir, _ := path.Split(fileDir)

	data, _ := ioutil.ReadFile(dir + fileName)

	// Create a new module from the file with the lib.
	module, _ := wasmer.NewModule(
		store,
		data,
	)

	wasiEnv, _ := wasmer.NewWasiStateBuilder("wasi-test-program").Environment("RUST_BACKTRACE", "full").Finalize()
	importObject, _ := wasiEnv.GenerateImportObject(store, module)
	instance, _ := wasmer.NewInstance(module, importObject)

	// result := callFunction(instance, "__jsonnet_internal_meta_exec", []byte{})
	// var paramNames []ast.Identifier
	// err := result.Lookup("res").Unmarshal(&paramNames)
	// fmt.Println(result)
	// fmt.Println(paramNames)
	// fmt.Println(err)
	var functionNames []string

	// lookup all function names in the module (ignore internal functions like allocate)
	for _, v := range module.Exports() {
		if v.Type().Kind().String() == "func" && !strings.HasPrefix(v.Name(), "__jsonnet_internal_") {
			functionNames = append(functionNames, v.Name())
		}
	}

	return instance, functionNames
}

func makeWASMFunction(functionName string, wasmerInstance *wasmer.Instance) *wasmFunction {
	metadataFunction := fmt.Sprintf("__jsonnet_internal_meta_%v", functionName)
	result := callFunction(wasmerInstance, metadataFunction, []byte{})
	var paramNames []ast.Identifier
	_ = result.Lookup("res").Unmarshal(&paramNames)

	return &wasmFunction{
		functionName:   functionName,
		wasmerInstance: wasmerInstance,
		params:         paramNames,
	}
}

func (wasmFunction *wasmFunction) evalCall(arguments callArguments, i *interpreter) (value, error) {
	fmt.Printf("%+v\n", arguments)
	flatArgs := flattenArgs(arguments, wasmFunction.parameters(), []value{})
	if len(flatArgs) > 0 {
		wasmArgs := make([]interface{}, 0, len(flatArgs))
		for _, arg := range flatArgs {
			v, err := i.evaluatePV(arg)
			if err != nil {
				return nil, err
			}
			json, err := i.manifestJSON(v)
			if err != nil {
				return nil, err
			}
			wasmArgs = append(wasmArgs, json)
		}
		fmt.Println(wasmArgs)
		marshalledArgs, _ := bson.Marshal(map[string]interface{}{"args": wasmArgs})
		result := wasmFunction.callFunction(wasmFunction.functionName, marshalledArgs)
		fmt.Println(result)
	} else {
		result := wasmFunction.callFunction(wasmFunction.functionName, []byte{})
		fmt.Println(result)
	}

	return makeValueBoolean(false), nil
}

func (wasmFunction *wasmFunction) parameters() []namedParameter {
	ret := make([]namedParameter, len(wasmFunction.params))
	for i := range ret {
		ret[i].name = wasmFunction.params[i]
	}
	return ret
}
