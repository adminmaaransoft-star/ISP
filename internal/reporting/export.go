package reporting

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Report generation shared by both delivery paths — FR-RPT-002 | MDS §4.8.
//
// One place that turns (report, filters) into bytes, so the CSV a user
// downloads now and the CSV a scheduled export delivers tonight are produced
// by the same code. Two generators would drift, and the drift would surface as
// an operator insisting the numbers changed.

// DefaultMonths bounds a time-series report when the caller names no window.
// Twelve gives a year-on-year comparison without scanning the whole history on
// every dashboard load.
const DefaultMonths = 12

// MaxMonths caps the window. Beyond this the response stops being a report and
// becomes a data export that should go through the asynchronous path.
const MaxMonths = 120

// Querier reads the reporting views. Satisfied by *db.ReportingStore.
type Querier interface {
	PlanMix(ctx context.Context, franchiseID *int) ([]PlanMixRow, error)
	GrowthMonthly(ctx context.Context, months int, franchiseID *int) ([]GrowthRow, error)
	TicketResolution(ctx context.Context, months int, franchiseID *int) ([]TicketResolutionRow, error)
	FranchiseCollection(ctx context.Context, months int, franchiseID *int) ([]CollectionRow, error)
}

// Filter narrows a report.
//
// FranchiseID is a pointer because nil and zero mean different things: nil is
// "every franchise", which only ISP-wide staff may ask for. Callers derive it
// from the token, never from a query parameter.
type Filter struct {
	Months      int
	FranchiseID *int
}

// NormaliseMonths clamps a requested window into range.
func NormaliseMonths(months int) int {
	if months <= 0 {
		return DefaultMonths
	}
	if months > MaxMonths {
		return MaxMonths
	}
	return months
}

// WriteCSV renders report to w.
//
// The rows are fetched whole and then written, rather than streamed row by row
// from the database. The store's methods already return slices, and reshaping
// them into cursors for a report bounded at ten years of monthly aggregates —
// thousands of rows, not millions — would be complexity bought for nothing.
func WriteCSV(ctx context.Context, q Querier, report string, f Filter, w io.Writer) error {
	months := NormaliseMonths(f.Months)

	switch report {
	case ReportPlanMix:
		// Plan mix is a point-in-time snapshot of the base, so it takes no
		// month window — passing one would imply a history it does not have.
		rows, err := q.PlanMix(ctx, f.FranchiseID)
		if err != nil {
			return err
		}
		return WritePlanMixCSV(w, rows)

	case ReportGrowth:
		rows, err := q.GrowthMonthly(ctx, months, f.FranchiseID)
		if err != nil {
			return err
		}
		return WriteGrowthCSV(w, rows)

	case ReportTicketResolution:
		rows, err := q.TicketResolution(ctx, months, f.FranchiseID)
		if err != nil {
			return err
		}
		return WriteTicketResolutionCSV(w, rows)

	case ReportCollection:
		rows, err := q.FranchiseCollection(ctx, months, f.FranchiseID)
		if err != nil {
			return err
		}
		return WriteCollectionCSV(w, rows)

	default:
		return fmt.Errorf("reporting: unknown report %q", report)
	}
}

// Filename builds the download name for a report.
//
// Dated, and scoped when the caller is franchise-bound, so a folder of
// downloads stays identifiable: "growth.csv" three times over is a filing
// problem the browser solves by appending (1) and (2).
func Filename(report string, f Filter, at time.Time) string {
	name := report + "-" + at.UTC().Format("2006-01-02")
	if f.FranchiseID != nil {
		name = fmt.Sprintf("%s-franchise-%d", name, *f.FranchiseID)
	}
	return name + ".csv"
}
