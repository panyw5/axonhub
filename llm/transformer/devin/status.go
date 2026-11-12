package devin

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/looplj/axonhub/llm/httpclient"
	"google.golang.org/protobuf/encoding/protowire"
)

const userStatusPath = "/exa.seat_management_pb.SeatManagementService/GetUserStatus"

type UserStatus struct {
	Plan                            string
	DailyRemaining, WeeklyRemaining *int64
	DailyReset, WeeklyReset         *time.Time
}

func UserStatusRequest(baseURL, token string) *httpclient.Request {
	if baseURL == "" {
		baseURL = DevinDefaultBaseURL
	}
	token = NormalizeToken(token)
	metadata := BuildDevinClientMetadataBytes(token, "", "")
	body := protowire.AppendTag(nil, 1, protowire.BytesType)
	body = protowire.AppendBytes(body, metadata)
	return &httpclient.Request{Method: http.MethodPost, URL: strings.TrimRight(baseURL, "/") + userStatusPath,
		ContentType: "application/proto", Body: body, DisableCompression: true, Headers: http.Header{
			"Authorization": {"Basic " + token + "-" + token}, "Connect-Protocol-Version": {"1"},
			"Content-Type": {"application/proto"}, "Accept": {"*/*"}, "Accept-Encoding": {"identity"}, "User-Agent": {""},
		},
	}
}

type protoField struct {
	num   protowire.Number
	typ   protowire.Type
	bytes []byte
	value uint64
}

func visitFields(data []byte, visit func(protoField) error) error {
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return protowire.ParseError(n)
		}
		data = data[n:]
		f := protoField{num: num, typ: typ}
		switch typ {
		case protowire.BytesType:
			f.bytes, n = protowire.ConsumeBytes(data)
		case protowire.VarintType:
			f.value, n = protowire.ConsumeVarint(data)
		default:
			n = protowire.ConsumeFieldValue(num, typ, data)
		}
		if n < 0 {
			return protowire.ParseError(n)
		}
		data = data[n:]
		if err := visit(f); err != nil {
			return err
		}
	}
	return nil
}

func ParseUserStatus(body []byte) (*UserStatus, error) {
	status := &UserStatus{}
	err := visitFields(body, func(root protoField) error {
		if root.num != 1 || root.typ != protowire.BytesType {
			return nil
		}
		return visitFields(root.bytes, func(user protoField) error {
			if user.num != 13 || user.typ != protowire.BytesType {
				return nil
			}
			return visitFields(user.bytes, func(plan protoField) error {
				if plan.num == 1 && plan.typ == protowire.BytesType {
					return visitFields(plan.bytes, func(info protoField) error {
						if info.num == 2 && info.typ == protowire.BytesType {
							status.Plan = string(info.bytes)
						}
						return nil
					})
				}
				if plan.typ != protowire.VarintType {
					return nil
				}
				value := int64(plan.value)
				switch plan.num {
				case 14, 15:
					if value < 0 || value > 100 {
						return fmt.Errorf("invalid Devin quota percentage")
					}
					if plan.num == 14 {
						status.DailyRemaining = &value
					} else {
						status.WeeklyRemaining = &value
					}
				case 17, 18:
					if value > 0 {
						reset := time.Unix(value, 0).UTC()
						if plan.num == 17 {
							status.DailyReset = &reset
						} else {
							status.WeeklyReset = &reset
						}
					}
				}
				return nil
			})
		})
	})
	if err != nil {
		return nil, fmt.Errorf("parse Devin user status: %w", err)
	}
	if status.DailyRemaining == nil && status.WeeklyRemaining == nil {
		return nil, fmt.Errorf("Devin user status contains no quota windows")
	}
	return status, nil
}
