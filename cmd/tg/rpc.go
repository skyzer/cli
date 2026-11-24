package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tdp"
	"github.com/gotd/td/tg"
)

type rpcCatalog struct {
	names        map[string]uint32
	constructors map[uint32]func() bin.Object
	methods      map[string]reflect.Method
}

func rpcKey(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, ".", ""))
}

func telegramRPC() rpcCatalog {
	client := reflect.TypeFor[*tg.Client]()
	methods := make(map[string]reflect.Method, client.NumMethod())
	for i := range client.NumMethod() {
		method := client.Method(i)
		if method.Type.NumIn() >= 2 && method.Type.In(1) == reflect.TypeFor[context.Context]() &&
			method.Type.NumOut() == 2 && method.Type.Out(1) == reflect.TypeFor[error]() {
			methods[rpcKey(method.Name)] = method
		}
	}
	return rpcCatalog{tg.NamesMap(), tg.TypesConstructorMap(), methods}
}

func (c rpcCatalog) object(name string) (bin.Object, error) {
	id, ok := c.names[name]
	if !ok || c.constructors[id] == nil {
		return nil, fmt.Errorf("unknown Telegram schema name %q", name)
	}
	return c.constructors[id](), nil
}

func (c rpcCatalog) method(name string, request bin.Object) (reflect.Method, reflect.Value, error) {
	method, ok := c.methods[rpcKey(name)]
	if ok {
		fields := request.(tdp.Object).TypeInfo().Fields
		switch method.Type.NumIn() {
		case 2:
			if len(fields) == 0 {
				return method, reflect.Value{}, nil
			}
		case 3:
			value := reflect.ValueOf(request)
			if method.Type.In(2) == value.Type() {
				return method, value, nil
			}
			// gotd generates direct arguments for some single-field requests.
			if len(fields) == 1 {
				field := value.Elem().FieldByName(fields[0].Name)
				if field.IsValid() && field.Type() == method.Type.In(2) {
					return method, field, nil
				}
			}
		}
	}
	return reflect.Method{}, reflect.Value{}, fmt.Errorf("%q has no callable Telegram RPC binding", name)
}

type rpcField struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Optional bool   `json:"optional"`
}

type rpcDescription struct {
	Name       string     `json:"name"`
	ID         uint32     `json:"id"`
	Parameters []rpcField `json:"parameters"`
	Result     string     `json:"result,omitempty"`
}

func (c rpcCatalog) describe(name string) (rpcDescription, error) {
	request, err := c.object(name)
	if err != nil {
		return rpcDescription{}, err
	}
	info := request.(tdp.Object).TypeInfo()
	value := reflect.ValueOf(request).Elem()
	result := rpcDescription{Name: name, ID: info.ID, Parameters: make([]rpcField, 0, len(info.Fields))}
	for _, field := range info.Fields {
		member := value.FieldByName(field.Name)
		if member.IsValid() && field.SchemaName != "flags" && field.SchemaName != "flags2" {
			result.Parameters = append(result.Parameters, rpcField{field.SchemaName, member.Type().String(), field.Null})
		}
	}
	if method, _, err := c.method(name, request); err == nil {
		result.Result = method.Type.Out(0).String()
	}
	return result, nil
}

func jsonObject(data json.RawMessage) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("expected a JSON object")
	}
	return fields, nil
}

func (c rpcCatalog) decodeObject(data json.RawMessage, object bin.Object) error {
	fields, err := jsonObject(data)
	if err != nil {
		return err
	}
	value := reflect.ValueOf(object)
	for _, field := range object.(tdp.Object).TypeInfo().Fields {
		if field.SchemaName == "flags" || field.SchemaName == "flags2" {
			continue
		}
		argument, present := fields[field.SchemaName]
		if !present {
			if !field.Null {
				return fmt.Errorf("missing Telegram argument %q", field.SchemaName)
			}
			continue
		}
		member := value.Elem().FieldByName(field.Name)
		if err := c.decodeValue(argument, member); err != nil {
			return fmt.Errorf("argument %q: %w", field.SchemaName, err)
		}
		if setter := value.MethodByName("Set" + field.Name); field.Null && setter.IsValid() {
			setter.Call([]reflect.Value{member})
		}
		delete(fields, field.SchemaName)
	}
	if len(fields) != 0 {
		return fmt.Errorf("unrecognized Telegram arguments")
	}
	return nil
}

func (c rpcCatalog) decodeUnion(data json.RawMessage, into reflect.Value) error {
	fields, err := jsonObject(data)
	if err != nil {
		return err
	}
	var name string
	if err := json.Unmarshal(fields["_"], &name); err != nil || name == "" {
		return fmt.Errorf("expected a Telegram constructor name in the _ field")
	}
	object, err := c.object(name)
	if err != nil {
		return err
	}
	value := reflect.ValueOf(object)
	if !value.Type().AssignableTo(into.Type()) {
		return fmt.Errorf("constructor %q does not implement %s", name, into.Type())
	}
	delete(fields, "_")
	arguments, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if err := c.decodeObject(arguments, object); err != nil {
		return err
	}
	into.Set(value)
	return nil
}

func (c rpcCatalog) decodeValue(data json.RawMessage, into reflect.Value) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("omit optional arguments instead of passing null")
	}
	if !into.IsValid() || !into.CanSet() {
		return fmt.Errorf("unsupported Telegram field")
	}
	switch into.Kind() {
	case reflect.Interface:
		return c.decodeUnion(data, into)
	case reflect.Slice:
		if into.Type().Elem().Kind() != reflect.Uint8 {
			var items []json.RawMessage
			if err := json.Unmarshal(data, &items); err != nil {
				return err
			}
			result := reflect.MakeSlice(into.Type(), len(items), len(items))
			for i, entry := range items {
				if err := c.decodeValue(entry, result.Index(i)); err != nil {
					return fmt.Errorf("element %d: %w", i, err)
				}
			}
			into.Set(result)
			return nil
		}
	case reflect.Struct:
		if object, ok := into.Addr().Interface().(bin.Object); ok {
			return c.decodeObject(data, object)
		}
	case reflect.Pointer:
		object := reflect.New(into.Type().Elem())
		if err := c.decodeValue(data, object.Elem()); err != nil {
			return err
		}
		into.Set(object)
		return nil
	}
	return json.Unmarshal(data, into.Addr().Interface())
}

func (c rpcCatalog) invoke(ctx context.Context, api *tg.Client, name, params string) (any, error) {
	request, err := c.object(name)
	if err != nil {
		return nil, err
	}
	method, argument, err := c.method(name, request)
	if err != nil {
		return nil, err
	}
	if err := c.decodeObject(json.RawMessage(params), request); err != nil {
		return nil, err
	}
	arguments := []reflect.Value{reflect.ValueOf(api), reflect.ValueOf(ctx)}
	if argument.IsValid() {
		arguments = append(arguments, argument)
	}
	result := method.Func.Call(arguments)
	if !result[1].IsNil() {
		return nil, result[1].Interface().(error)
	}
	return result[0].Interface(), nil
}

func (a *app) newRPCCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "rpc",
		Short:   "Inspect or invoke the Telegram RPC methods in the installed schema",
		GroupID: groupMessaging,
	}
	describe := &cobra.Command{
		Use:   "describe <method-or-constructor>",
		Short: "Describe a Telegram TL method or constructor",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			description, err := telegramRPC().describe(args[0])
			if err != nil {
				return err
			}
			return a.printer.Emit(description)
		},
	}
	var confirmed bool
	invoke := &cobra.Command{
		Use:   "invoke <method> <params-json>",
		Short: "Execute a raw Telegram RPC as the current user",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !confirmed {
				return fmt.Errorf("raw RPCs can change account data; pass --yes to proceed")
			}
			return a.run(cmd.Context(), runParams{auth: authUser}, func(ctx context.Context, api *tg.Client) error {
				result, err := telegramRPC().invoke(ctx, api, args[0], args[1])
				if err != nil {
					return err
				}
				return a.printer.Emit(result)
			})
		},
	}
	invoke.Flags().BoolVar(&confirmed, "yes", false, "Confirm execution of the raw RPC")
	root.AddCommand(describe, invoke)
	return root
}
