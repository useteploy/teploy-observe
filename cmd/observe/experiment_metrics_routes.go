package main

// Experiment depth routes (migration 063): the per-experiment analysis
// settings and the ingest endpoint for count / mean / secondary metric
// observations. Registered from main.go with one RegisterExperimentMetricRoutes
// call.

import (
	"context"
	"math"

	"github.com/neutron-build/neutron/go/neutron"

	"github.com/useteploy/teploy-observe/internal/experiments"
	"github.com/useteploy/teploy-observe/internal/ingest"
)

// RegisterExperimentMetricRoutes wires GET/PUT settings (JWT; editor+ writes)
// and POST /experiments/metric (API key ingest).
func RegisterExperimentMetricRoutes(
	r *neutron.Router,
	ingestGroup *neutron.Router,
	jwtMW neutron.Middleware,
	requireEditor neutron.Middleware,
	svc *experiments.ExperimentService,
) {
	neutron.Post(ingestGroup, "/experiments/metric", experimentMetricHandler(svc),
		neutron.WithTags("experiments"),
		neutron.WithSummary("Record a count/mean/secondary experiment metric observation"))

	read := r.Group("/api/v1/experiments", jwtMW)
	write := read.Group("", requireEditor)
	neutron.Get(read, "/{experiment_id}/settings", getExperimentSettingsHandler(svc),
		neutron.WithTags("experiments"), neutron.WithSummary("Get experiment analysis settings"))
	neutron.Put(write, "/{experiment_id}/settings", putExperimentSettingsHandler(svc),
		neutron.WithTags("experiments"), neutron.WithSummary("Set experiment analysis settings (metric kind, secondary goals, horizon)"))
}

type experimentMetricInput struct {
	ExperimentID string  `json:"experiment_id"`
	UserID       string  `json:"user_id"`
	Metric       string  `json:"metric"`
	Value        float64 `json:"value"`
}

func experimentMetricHandler(svc *experiments.ExperimentService) neutron.HandlerFunc[experimentMetricInput, neutron.Empty] {
	return func(ctx context.Context, in experimentMetricInput) (neutron.Empty, error) {
		siteID := ingest.SiteIDFromContext(ctx)
		if siteID == "" || in.ExperimentID == "" || in.UserID == "" {
			return neutron.Empty{}, neutron.ErrBadRequest("experiment_id and user_id required")
		}
		if in.Metric == "" {
			in.Metric = experiments.PrimaryMetricKey
		}
		if math.IsNaN(in.Value) || math.IsInf(in.Value, 0) {
			return neutron.Empty{}, neutron.ErrBadRequest("value must be finite")
		}
		if err := svc.RecordMetric(ctx, in.ExperimentID, siteID, in.UserID, in.Metric, in.Value); err != nil {
			return neutron.Empty{}, neutron.ErrBadRequest(err.Error())
		}
		return neutron.Empty{}, nil
	}
}

type experimentSettingsGetInput struct {
	ExperimentID string `path:"experiment_id"`
	SiteID       string `query:"site_id"`
}

func getExperimentSettingsHandler(svc *experiments.ExperimentService) neutron.HandlerFunc[experimentSettingsGetInput, experiments.ExperimentConfig] {
	return func(ctx context.Context, in experimentSettingsGetInput) (experiments.ExperimentConfig, error) {
		if in.SiteID == "" {
			return experiments.ExperimentConfig{}, neutron.ErrBadRequest("site_id required")
		}
		return svc.LoadConfig(ctx, in.ExperimentID, in.SiteID)
	}
}

type experimentSettingsPutInput struct {
	ExperimentID string `path:"experiment_id"`
	SiteID       string `json:"site_id"`
	// Fields mirror experiments.ExperimentConfig (explicit, not embedded, so
	// the binder sees plain json tags).
	MetricKind          string                      `json:"metric_kind"`
	WinsorizePct        float64                     `json:"winsorize_pct"`
	PlannedSamplePerArm int                         `json:"planned_sample_per_arm"`
	AllowEarlyWinner    bool                        `json:"allow_early_winner"`
	Secondary           []experiments.SecondaryGoal `json:"secondary_goals"`
}

func putExperimentSettingsHandler(svc *experiments.ExperimentService) neutron.HandlerFunc[experimentSettingsPutInput, experiments.ExperimentConfig] {
	return func(ctx context.Context, in experimentSettingsPutInput) (experiments.ExperimentConfig, error) {
		if in.SiteID == "" {
			return experiments.ExperimentConfig{}, neutron.ErrBadRequest("site_id required")
		}
		cfg, err := svc.SaveConfig(ctx, in.ExperimentID, in.SiteID, experiments.ExperimentConfig{
			MetricKind: in.MetricKind, WinsorizePct: in.WinsorizePct,
			PlannedSamplePerArm: in.PlannedSamplePerArm, AllowEarlyWinner: in.AllowEarlyWinner,
			Secondary: in.Secondary,
		})
		if err != nil {
			return experiments.ExperimentConfig{}, neutron.ErrBadRequest(err.Error())
		}
		return cfg, nil
	}
}
