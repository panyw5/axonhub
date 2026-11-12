package biz

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/transformer/devin"
)

func TestDevinChannel(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(context.Background())
	ch := client.Channel.Create().SetName("Devin").SetType(channel.TypeDevin).
		SetCredentials(objects.ChannelCredentials{APIKey: "session-token"}).
		SetSupportedModels([]string{"swe-2"}).SetDefaultTestModel("swe-2").SaveX(ctx)
	service := NewChannelServiceForTest(client)
	built, err := service.buildChannelWithOutbounds(ch)
	require.NoError(t, err)
	require.Len(t, built.Outbounds, 1)
	require.Equal(t, llm.APIFormatDevinChat, built.Outbound.APIFormat())
	_, ok := built.Outbound.(pipeline.ChannelCustomizedExecutor)
	require.True(t, ok)
	require.Contains(t, llm.CapableAPIFormats(llm.RequestTypeChat), llm.APIFormatDevinChat.String())
	_, err = BuildOutboundByAPIFormat(built, llm.APIFormatDevinChat.String())
	require.NoError(t, err)
	ch.Endpoints = []objects.ChannelEndpoint{{APIFormat: llm.APIFormatDevinChat.String(), Path: "/custom/chat", BaseURL: "https://proxy.example.invalid"}}
	built, err = service.buildChannelWithOutbounds(ch)
	require.NoError(t, err)
	require.IsType(t, &devin.OutboundTransformer{}, built.Outbounds[llm.APIFormatDevinChat.String()])
	_, err = service.buildChannelWithTransformer(&ent.Channel{Type: channel.TypeDevin})
	require.Error(t, err)
	fetcher := NewModelFetcher(service.httpClient, service)
	models, err := fetcher.FetchModels(ctx, FetchModelsInput{ChannelType: "devin"})
	require.NoError(t, err)
	require.NotEmpty(t, models.Models)
}
