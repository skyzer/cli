package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

func TestRuntimeRPCDisablesAutomaticAuthorizationAndUpdates(t *testing.T) {
	for _, params := range []runParams{
		{layer: 234, auth: authBot},
		{layer: 234, updates: true},
		{layer: 234, authorize: func(context.Context, *telegram.Client, tg.UpdateDispatcher) error {
			t.Fatal("runtime RPC attempted automatic authorization")
			return nil
		}},
		{linkedAppID: 12345},
	} {
		require.ErrorContains(t, newApp().connectWith(t.Context(), nil, params, nil),
			"runtime schema connections require an existing user session without updates")
	}
}

func TestRPCListWithoutConfig(t *testing.T) {
	root := newRootCmd()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetArgs([]string{"--config", filepath.Join(t.TempDir(), "missing.yaml"),
		"--output", "json", "rpc", "list"})
	require.NoError(t, root.Execute())

	var response struct {
		Data []string `json:"data"`
	}
	require.NoError(t, json.Unmarshal(output.Bytes(), &response))
	require.True(t, sort.StringsAreSorted(response.Data))
	require.Contains(t, response.Data, "help.getConfig")
	require.Contains(t, response.Data, "messages.sendMedia")
	require.NotContains(t, response.Data, "inputPeerSelf")
}

func TestRPCDescribe(t *testing.T) {
	config, err := telegramRPC().describe("help.getConfig")
	require.NoError(t, err)
	require.Equal(t, uint32(tg.HelpGetConfigRequestTypeID), config.ID)
	require.Empty(t, config.Parameters)
	require.Equal(t, "*tg.Config", config.Result)

	send, err := telegramRPC().describe("messages.sendMedia")
	require.NoError(t, err)
	require.Contains(t, send.Parameters, rpcField{Name: "peer", Type: "tg.InputPeerClass"})
	require.Contains(t, send.Parameters, rpcField{Name: "silent", Type: "bool", Optional: true})
	require.NotEmpty(t, send.Result)
}

func TestRPCDescribeWithoutConfig(t *testing.T) {
	root := newRootCmd()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetArgs([]string{"--config", filepath.Join(t.TempDir(), "missing.yaml"),
		"--output", "json", "rpc", "describe", "messages.sendMedia"})
	require.NoError(t, root.Execute())

	var response struct {
		Data rpcDescription `json:"data"`
	}
	require.NoError(t, json.Unmarshal(output.Bytes(), &response))
	require.Equal(t, "messages.sendMedia", response.Data.Name)
}

func TestRPCInvokeMediaWithSchemaNames(t *testing.T) {
	var verified bool
	api := newFuncAPI(t, func(input bin.Encoder) (bin.Encoder, error) {
		request := input.(*tg.MessagesSendMediaRequest)
		require.IsType(t, &tg.InputPeerSelf{}, request.Peer)
		media := request.Media.(*tg.InputMediaUploadedDocument)
		require.Equal(t, "audio/mpeg", media.MimeType)
		require.Equal(t, "clip.mp3", media.File.(*tg.InputFile).Name)
		require.Equal(t, int64(901234567890123456), request.RandomID)
		require.Equal(t, "Test clip", request.Message)
		verified = true
		return &tg.UpdatesTooLong{}, nil
	})
	result, err := telegramRPC().invoke(context.Background(), api, "messages.sendMedia", `{
		"peer":{"_":"inputPeerSelf"},
		"media":{"_":"inputMediaUploadedDocument",
			"file":{"_":"inputFile","id":1,"parts":1,"name":"clip.mp3","md5_checksum":""},
			"mime_type":"audio/mpeg",
			"attributes":[{"_":"documentAttributeFilename","file_name":"clip.mp3"}]},
		"message":"Test clip","random_id":901234567890123456
	}`)
	require.NoError(t, err)
	require.True(t, verified)
	require.IsType(t, &tg.UpdatesTooLong{}, result)
}

func TestRPCInvokeDirectArgument(t *testing.T) {
	api := newFuncAPI(t, func(input bin.Encoder) (bin.Encoder, error) {
		request := input.(*tg.UsersGetFullUserRequest)
		require.IsType(t, &tg.InputUserSelf{}, request.ID)
		return &tg.UsersUserFull{}, nil
	})
	result, err := telegramRPC().invoke(context.Background(), api, "users.getFullUser",
		`{"id":{"_":"inputUserSelf"}}`)
	require.NoError(t, err)
	require.IsType(t, &tg.UsersUserFull{}, result)
}

func TestRPCInvokeWithExactWideIntegers(t *testing.T) {
	api := newFuncAPI(t, func(input bin.Encoder) (bin.Encoder, error) {
		request := input.(*tg.MessagesSendMessageRequest)
		peer := request.Peer.(*tg.InputPeerChannel)
		require.Equal(t, int64(9007199254740993), peer.ChannelID)
		require.Equal(t, int64(-9223372036854775808), peer.AccessHash)
		require.Equal(t, int64(9223372036854775807), request.RandomID)
		return &tg.UpdatesTooLong{}, nil
	})
	result, err := telegramRPC().invoke(context.Background(), api, "messages.sendMessage", `{
		"peer":{"_":"inputPeerChannel","channel_id":"9007199254740993","access_hash":"-9223372036854775808"},
		"message":"A test message", "random_id":"9223372036854775807"
	}`)
	require.NoError(t, err)
	require.IsType(t, &tg.UpdatesTooLong{}, result)
}

func TestRPCRejectsInvalidRequests(t *testing.T) {
	api := newFuncAPI(t, func(_ bin.Encoder) (bin.Encoder, error) {
		t.Fatal("invalid RPC reached the network")
		return nil, nil
	})
	for _, tc := range []struct {
		method string
		args   string
	}{
		{"bogus.method", "{}"},
		{"inputPeerSelf", "{}"},
		{"help.getConfig", `{"unexpected":1}`},
		{"messages.sendMedia", `{"peer":{"_":"inputFile"}}`},
		{"messages.sendMedia", `{"peer":null}`},
		{"messages.sendMessage", `{"peer":{"_":"inputPeerSelf"},"message":"test","random_id":"9223372036854775808"}`},
		{"users.getUsers", `{"id":[{"_":"inputUserSelf"},{"_":"inputPeerSelf"}]}`},
	} {
		t.Run(tc.method+tc.args, func(t *testing.T) {
			_, err := telegramRPC().invoke(context.Background(), api, tc.method, tc.args)
			require.Error(t, err)
		})
	}
}
