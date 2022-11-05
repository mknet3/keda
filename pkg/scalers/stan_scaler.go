package scalers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-logr/logr"
	v2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/metrics/pkg/apis/external_metrics"

	kedautil "github.com/kedacore/keda/v2/pkg/util"
)

type monitorChannelInfo struct {
	Name         string                  `json:"name"`
	MsgCount     int64                   `json:"msgs"`
	LastSequence int64                   `json:"last_seq"`
	Subscriber   []monitorSubscriberInfo `json:"subscriptions"`
}

type monitorSubscriberInfo struct {
	ClientID     string `json:"client_id"`
	QueueName    string `json:"queue_name"`
	Inbox        string `json:"inbox"`
	AckInbox     string `json:"ack_inbox"`
	IsDurable    bool   `json:"is_durable"`
	IsOffline    bool   `json:"is_offline"`
	MaxInflight  int    `json:"max_inflight"`
	LastSent     int64  `json:"last_sent"`
	PendingCount int    `json:"pending_count"`
	IsStalled    bool   `json:"is_stalled"`
}

type stanScaler struct {
	channelInfo *monitorChannelInfo
	metricType  v2.MetricTargetType
	metadata    stanMetadata
	httpClient  *http.Client
	logger      logr.Logger
}

type stanMetadata struct {
	natsServerMonitoringEndpoint string
	useHttps                     bool
	queueGroup                   string
	durableName                  string
	subject                      string
	lagThreshold                 int64
	activationLagThreshold       int64
	scalerIndex                  int
}

const (
	stanMetricType             = "External"
	defaultStanLagThreshold    = 10
	natsStreamingHttpProtocol  = "http"
	natsStreamingHttpsProtocol = "https"
)

// NewStanScaler creates a new stanScaler
func NewStanScaler(config *ScalerConfig) (Scaler, error) {
	metricType, err := GetMetricTargetType(config)
	if err != nil {
		return nil, fmt.Errorf("error getting scaler metric type: %s", err)
	}

	stanMetadata, err := parseStanMetadata(config)
	if err != nil {
		return nil, fmt.Errorf("error parsing stan metadata: %s", err)
	}

	return &stanScaler{
		channelInfo: &monitorChannelInfo{},
		metricType:  metricType,
		metadata:    stanMetadata,
		httpClient:  kedautil.CreateHTTPClient(config.GlobalHTTPTimeout, false),
		logger:      InitializeLogger(config, "stan_scaler"),
	}, nil
}

func parseStanMetadata(config *ScalerConfig) (stanMetadata, error) {
	meta := stanMetadata{}
	var err error
	meta.natsServerMonitoringEndpoint, err = GetFromAuthOrMeta(config, "natsServerMonitoringEndpoint")
	if err != nil {
		return meta, err
	}

	if config.TriggerMetadata["queueGroup"] == "" {
		return meta, errors.New("no queue group given")
	}
	meta.queueGroup = config.TriggerMetadata["queueGroup"]

	if config.TriggerMetadata["durableName"] == "" {
		return meta, errors.New("no durable name group given")
	}
	meta.durableName = config.TriggerMetadata["durableName"]

	if config.TriggerMetadata["subject"] == "" {
		return meta, errors.New("no subject given")
	}
	meta.subject = config.TriggerMetadata["subject"]

	meta.lagThreshold = defaultStanLagThreshold

	if val, ok := config.TriggerMetadata[lagThresholdMetricName]; ok {
		t, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return meta, fmt.Errorf("error parsing %s: %s", lagThresholdMetricName, err)
		}
		meta.lagThreshold = t
	}

	meta.activationLagThreshold = 0
	if val, ok := config.TriggerMetadata["activationLagThreshold"]; ok {
		activationTargetQueryValue, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return meta, fmt.Errorf("activationLagThreshold parsing error %s", err.Error())
		}
		meta.activationLagThreshold = activationTargetQueryValue
	}

	meta.scalerIndex = config.ScalerIndex

	meta.useHttps = false
	if val, ok := config.TriggerMetadata["useHttps"]; ok {
		useHttps, err := strconv.ParseBool(val)
		if err != nil {
			return meta, fmt.Errorf("useHttps parsing error %s", err.Error())
		}
		meta.useHttps = useHttps
	}

	return meta, nil
}

// IsActive determines if we need to scale from zero
func (s *stanScaler) IsActive(ctx context.Context) (bool, error) {
	monitoringEndpoint := s.getMonitoringEndpoint()

	req, err := http.NewRequestWithContext(ctx, "GET", monitoringEndpoint, nil)
	if err != nil {
		return false, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		s.logger.Error(err, "Unable to access the nats streaming broker monitoring endpoint", "natsServerMonitoringEndpoint", s.metadata.natsServerMonitoringEndpoint)
		return false, err
	}

	if resp.StatusCode == 404 {
		req, err := http.NewRequestWithContext(ctx, "GET", s.getSTANChannelsEndpoint(), nil)
		if err != nil {
			return false, err
		}
		baseResp, err := s.httpClient.Do(req)
		if err != nil {
			return false, err
		}
		defer baseResp.Body.Close()
		if baseResp.StatusCode == 404 {
			s.logger.Info("Streaming broker endpoint returned 404. Please ensure it has been created", "url", monitoringEndpoint, "channelName", s.metadata.subject)
		} else {
			s.logger.Info("Unable to connect to STAN. Please ensure you have configured the ScaledObject with the correct endpoint.", "baseResp.StatusCode", baseResp.StatusCode, "natsServerMonitoringEndpoint", s.metadata.natsServerMonitoringEndpoint)
		}

		return false, err
	}

	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&s.channelInfo); err != nil {
		s.logger.Error(err, "Unable to decode channel info as %v", err)
		return false, err
	}
	return s.hasPendingMessage() || s.getMaxMsgLag() > s.metadata.activationLagThreshold, nil
}

func (s *stanScaler) getSTANChannelsEndpoint() string {
	protocol := natsStreamingHttpProtocol
	if s.metadata.useHttps {
		protocol = natsStreamingHttpsProtocol
	}
	return fmt.Sprintf("%s://%s/streaming/channelsz", protocol, s.metadata.natsServerMonitoringEndpoint)
}

func (s *stanScaler) getMonitoringEndpoint() string {
	return s.getSTANChannelsEndpoint() + "?channel=" + s.metadata.subject + "&subs=1"
}

func (s *stanScaler) getMaxMsgLag() int64 {
	maxValue := int64(0)
	combinedQueueName := s.metadata.durableName + ":" + s.metadata.queueGroup

	for _, subs := range s.channelInfo.Subscriber {
		if subs.LastSent > maxValue && subs.QueueName == combinedQueueName {
			maxValue = subs.LastSent
		}
	}

	return s.channelInfo.LastSequence - maxValue
}

func (s *stanScaler) hasPendingMessage() bool {
	subscriberFound := false
	combinedQueueName := s.metadata.durableName + ":" + s.metadata.queueGroup

	for _, subs := range s.channelInfo.Subscriber {
		if subs.QueueName == combinedQueueName {
			subscriberFound = true

			if subs.PendingCount > 0 {
				return true
			}

			break
		}
	}

	if !subscriberFound {
		s.logger.Info("The STAN subscription was not found.", "combinedQueueName", combinedQueueName)
	}

	return false
}

func (s *stanScaler) GetMetricSpecForScaling(context.Context) []v2.MetricSpec {
	metricName := kedautil.NormalizeString(fmt.Sprintf("stan-%s", s.metadata.subject))
	externalMetric := &v2.ExternalMetricSource{
		Metric: v2.MetricIdentifier{
			Name: GenerateMetricNameWithIndex(s.metadata.scalerIndex, metricName),
		},
		Target: GetMetricTarget(s.metricType, s.metadata.lagThreshold),
	}
	metricSpec := v2.MetricSpec{
		External: externalMetric, Type: stanMetricType,
	}
	return []v2.MetricSpec{metricSpec}
}

// GetMetrics returns value for a supported metric and an error if there is a problem getting the metric
func (s *stanScaler) GetMetrics(ctx context.Context, metricName string, metricSelector labels.Selector) ([]external_metrics.ExternalMetricValue, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", s.getMonitoringEndpoint(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpClient.Do(req)

	if err != nil {
		s.logger.Error(err, "Unable to access the nats streaming broker monitoring endpoint", "natsServerMonitoringEndpoint", s.metadata.natsServerMonitoringEndpoint)
		return []external_metrics.ExternalMetricValue{}, err
	}

	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&s.channelInfo); err != nil {
		s.logger.Error(err, "Unable to decode channel info as %v", err)
		return []external_metrics.ExternalMetricValue{}, err
	}
	totalLag := s.getMaxMsgLag()
	s.logger.V(1).Info("Stan scaler: Providing metrics based on totalLag, threshold", "totalLag", totalLag, "lagThreshold", s.metadata.lagThreshold)
	metric := GenerateMetricInMili(metricName, float64(totalLag))
	return append([]external_metrics.ExternalMetricValue{}, metric), nil
}

// Nothing to close here.
func (s *stanScaler) Close(context.Context) error {
	return nil
}
