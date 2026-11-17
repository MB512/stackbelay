package store

import (
	"context"
	"errors"
	"testing"

	"github.com/MB512/stackvault/internal/jobs"
)

func TestSchedules(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.SyncStacks(ctx, []string{"web", "db"}); err != nil {
		t.Fatal(err)
	}
	web, err := s.StackByName(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	db, err := s.StackByName(ctx, "db")
	if err != nil {
		t.Fatal(err)
	}

	w1, err := s.CreateSchedule(ctx, Schedule{StackID: web.ID, CronExpr: "0 3 * * *", Type: jobs.TypeUpdate, PreStop: true, RollbackInPlace: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSchedule(ctx, Schedule{StackID: db.ID, CronExpr: "@hourly", Type: jobs.TypeBackup}); err != nil {
		t.Fatal(err)
	}
	nb, err := s.CreateSchedule(ctx, Schedule{StackID: db.ID, CronExpr: "@weekly", Type: jobs.TypeUpdate, NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	if sc, err := s.ScheduleByID(ctx, nb); err != nil || !sc.NoBackup || sc.Type != jobs.TypeUpdate {
		t.Fatalf("update without backup: %+v, %v", sc, err)
	}
	if err := s.DeleteSchedule(ctx, db.ID, nb); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSchedule(ctx, Schedule{StackID: 999, CronExpr: "@daily", Type: jobs.TypeBackup}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown stack: err = %v, want ErrNotFound", err)
	}
	for _, bad := range []Schedule{
		{StackID: db.ID, CronExpr: "@daily", Type: jobs.TypeRestore},
		{StackID: db.ID, CronExpr: "@daily"},
		{StackID: db.ID, CronExpr: "@daily", Type: jobs.TypeBackup, RollbackInPlace: true},
		{StackID: db.ID, CronExpr: "@daily", Type: jobs.TypeBackup, NoBackup: true},
		{StackID: db.ID, CronExpr: "@daily", Type: jobs.TypeUpdate, NoBackup: true, RollbackInPlace: true},
		{StackID: db.ID, CronExpr: "@daily", Type: jobs.TypeUpdate, NoBackup: true, PreStop: true},
	} {
		if _, err := s.CreateSchedule(ctx, bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}

	all, err := s.ListSchedules(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].StackName != "db" || all[1].StackName != "web" {
		t.Fatalf("all schedules = %+v, want db then web", all)
	}
	got := all[1]
	want := Schedule{ID: w1, StackID: web.ID, StackName: "web", CronExpr: "0 3 * * *", Type: jobs.TypeUpdate, PreStop: true,
		RollbackInPlace: true, Enabled: true}
	if got != want {
		t.Errorf("schedule = %+v, want %+v", got, want)
	}

	if byID, err := s.ScheduleByID(ctx, w1); err != nil || byID != want {
		t.Errorf("ScheduleByID = %+v, %v; want %+v", byID, err, want)
	}

	// A schedule can only be changed through its own stack.
	if err := s.SetScheduleEnabled(ctx, db.ID, w1, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("enable via other stack: err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteSchedule(ctx, db.ID, w1); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete via other stack: err = %v, want ErrNotFound", err)
	}

	if err := s.SetScheduleEnabled(ctx, web.ID, w1, false); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSchedules(ctx, web.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Enabled {
		t.Errorf("web schedules after disable = %+v", list)
	}

	if err := s.DeleteSchedule(ctx, web.ID, w1); err != nil {
		t.Fatal(err)
	}
	if list, err = s.ListSchedules(ctx, web.ID); err != nil || len(list) != 0 {
		t.Errorf("web schedules after delete = %+v, %v", list, err)
	}
	if _, err := s.ScheduleByID(ctx, w1); !errors.Is(err, ErrNotFound) {
		t.Errorf("ScheduleByID after delete: err = %v, want ErrNotFound", err)
	}
}
