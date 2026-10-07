package ackhandler

import (
	"testing"

	"github.com/quic-go/quic-go/internal/congestion"
	"github.com/quic-go/quic-go/internal/mocks"
	"github.com/quic-go/quic-go/internal/monotime"
	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/quic-go/quic-go/internal/utils"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestSentPacketHandlerCongestionFactory(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	rttStats := utils.NewRTTStats()
	var created []*mocks.MockSendAlgorithmWithDebugInfos
	factory := func(r *utils.RTTStats, initialMaxDatagramSize protocol.ByteCount) congestion.SendAlgorithmWithDebugInfos {
		require.Same(t, rttStats, r)
		require.Equal(t, protocol.ByteCount(1234), initialMaxDatagramSize)
		cong := mocks.NewMockSendAlgorithmWithDebugInfos(mockCtrl)
		created = append(created, cong)
		return cong
	}
	sph := NewSentPacketHandler(
		0,
		1234,
		rttStats,
		&utils.ConnectionStats{},
		false,
		false,
		nil,
		protocol.PerspectiveClient,
		nil,
		utils.DefaultLogger,
		factory,
	)
	require.Len(t, created, 1)
	created[0].EXPECT().TimeUntilSend(gomock.Any()).Return(monotime.Time(42))
	require.Equal(t, monotime.Time(42), sph.TimeUntilSend())

	// a path migration creates a new controller
	sph.MigratedPath(monotime.Now(), 1234)
	require.Len(t, created, 2)
	created[1].EXPECT().TimeUntilSend(gomock.Any()).Return(monotime.Time(43))
	require.Equal(t, monotime.Time(43), sph.TimeUntilSend())
}

func TestSentPacketHandlerNilCongestionFactory(t *testing.T) {
	sph := NewSentPacketHandler(
		0,
		1200,
		utils.NewRTTStats(),
		&utils.ConnectionStats{},
		false,
		false,
		nil,
		protocol.PerspectiveClient,
		nil,
		utils.DefaultLogger,
		nil, // NewReno
	)
	require.NotNil(t, sph.(*sentPacketHandler).congestion)
	require.Equal(t, SendAny, sph.SendMode(monotime.Now()))
}
