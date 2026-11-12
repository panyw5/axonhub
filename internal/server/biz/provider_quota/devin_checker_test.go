package provider_quota

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestDevinQuotaChecker(t *testing.T) {
	wrap := func(num protowire.Number, data []byte) []byte {
		return protowire.AppendBytes(protowire.AppendTag(nil, num, protowire.BytesType), data)
	}
	for _, daily := range []uint64{0, 15, 80} {
		t.Run(fmt.Sprintf("daily_remaining_%d", daily), func(t *testing.T) {
			plan := protowire.AppendVarint(protowire.AppendTag(nil, 14, protowire.VarintType), daily)
			plan = protowire.AppendVarint(protowire.AppendTag(plan, 15, protowire.VarintType), 90)
			plan = protowire.AppendVarint(protowire.AppendTag(plan, 17, protowire.VarintType), 2000000000)
			plan = protowire.AppendVarint(protowire.AppendTag(plan, 18, protowire.VarintType), 2000100000)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Contains(t, r.URL.Path, "GetUserStatus")
				require.Equal(t, "application/proto", r.Header.Get("Content-Type"))
				require.Empty(t, r.Header.Get("User-Agent"))
				require.Equal(t, "Basic devin-session-token$test-devin-session-token$test", r.Header.Get("Authorization"))
				_, _ = w.Write(wrap(1, wrap(13, plan)))
			}))
			defer server.Close()
			checker := NewDevinQuotaChecker(httpclient.NewHttpClient())
			ch := &ent.Channel{Type: channel.TypeDevin, BaseURL: server.URL, Credentials: objects.ChannelCredentials{APIKeys: []string{"test"}}}
			require.True(t, checker.SupportsChannel(ch))
			data, err := checker.CheckQuota(context.Background(), ch)
			require.NoError(t, err)
			require.Len(t, data.Limits, 2)
			require.Equal(t, "daily", data.Limits[0].Window)
			require.InDelta(t, 1-float64(daily)/100, data.Limits[0].UsageRatio, 0.001)
			require.Equal(t, 24*time.Hour, data.Limits[0].NextResetAt.Sub(*data.Limits[0].PeriodStart))
			if daily == 0 {
				require.Equal(t, "exhausted", data.Status)
				require.False(t, data.Ready)
			} else {
				require.True(t, data.Ready)
			}
		})
	}
}

func TestDevinQuotaRejectsInvalidCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer server.Close()
	checker := NewDevinQuotaChecker(httpclient.NewHttpClient())
	ch := &ent.Channel{Type: channel.TypeDevin, BaseURL: server.URL, Credentials: objects.ChannelCredentials{APIKey: "test"}}
	_, err := checker.CheckQuota(context.Background(), ch)
	require.ErrorIs(t, err, ErrInvalidCredentials)
	ch.Credentials = objects.ChannelCredentials{APIKeys: []string{"one", "two"}}
	_, err = checker.CheckQuota(context.Background(), ch)
	require.ErrorContains(t, err, "one session token")
}
