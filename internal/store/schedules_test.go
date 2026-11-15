package store

import (
	"context"
	"errors"
	"testing"
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

	w1, err := s.CreateSchedule(ctx, web.ID, "0 3 * * *", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSchedule(ctx, db.ID, "@hourly", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSchedule(ctx, 999, "@daily", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown stack: err = %v, want ErrNotFound", err)
	}

	all, err := s.ListSchedules(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].StackName != "db" || all[1].StackName != "web" {
		t.Fatalf("all schedules = %+v, want db then web", all)
	}
	got := all[1]
	want := Schedule{ID: w1, StackID: web.ID, StackName: "web", CronExpr: "0 3 * * *", PreStop: true, Enabled: true}
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
