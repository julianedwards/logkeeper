package model

import (
	"context"
	"testing"
	"time"

	"github.com/evergreen-ci/logkeeper/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindBuildByBuilder(t *testing.T) {
	require.NoError(t, testutil.InitDB())
	require.NoError(t, testutil.ClearCollections(BuildsCollection))

	b0 := Build{
		Id:       "b0",
		Builder:  "builder0",
		BuildNum: 0,
	}
	require.NoError(t, b0.Insert())

	b1 := Build{
		Id:       "b1",
		Builder:  "builder1",
		BuildNum: 0,
	}
	require.NoError(t, b1.Insert())

	b, err := FindBuildByBuilder(b0.Builder, b0.BuildNum)
	assert.NoError(t, err)
	assert.Equal(t, b0.Id, b.Id)
}

func TestFindBuildById(t *testing.T) {
	require.NoError(t, testutil.InitDB())
	require.NoError(t, testutil.ClearCollections(BuildsCollection))

	b0 := Build{Id: "b0"}
	require.NoError(t, b0.Insert())

	b1 := Build{Id: "b1"}
	require.NoError(t, b1.Insert())

	b, err := FindBuildById(b0.Id)
	assert.NoError(t, err)
	assert.Equal(t, b0.Id, b.Id)
}

func TestUpdateFailedBuild(t *testing.T) {
	require.NoError(t, testutil.InitDB())
	require.NoError(t, testutil.ClearCollections(BuildsCollection))

	buildID := "b0"
	assert.NoError(t, (&Build{Id: buildID}).Insert())
	assert.NoError(t, UpdateFailedBuild(buildID))

	b, err := FindBuildById(buildID)
	assert.NoError(t, err)
	assert.Equal(t, buildID, b.Id)
	assert.True(t, b.Failed)
}

func TestIncrementBuildSequence(t *testing.T) {
	require.NoError(t, testutil.InitDB())
	require.NoError(t, testutil.ClearCollections(BuildsCollection))

	buildID := "b0"
	b := &Build{Id: buildID, Seq: 1}
	require.NoError(t, b.Insert())

	assert.NoError(t, b.IncrementSequence(1))
	assert.Equal(t, 2, b.Seq)

	b, err := FindBuildById(buildID)
	assert.NoError(t, err)
	assert.Equal(t, b.Seq, 2)
}

func TestStreamingGetOldBuilds(t *testing.T) {
	require.NoError(t, testutil.InitDB())
	require.NoError(t, testutil.ClearCollections(BuildsCollection))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	oldBuild := Build{
		Id:      "old_build",
		Started: time.Date(2009, time.November, 10, 0, 0, 0, 0, time.UTC),
		Info:    BuildInfo{TaskID: "t0"},
	}
	require.NoError(t, oldBuild.Insert())
	newBuild := Build{
		Id:      "new_build",
		Started: time.Now(),
		Info:    BuildInfo{TaskID: "t0"},
	}
	require.NoError(t, newBuild.Insert())
	failedBuild := Build{
		Id:      "failed_build",
		Started: time.Date(2009, time.November, 10, 0, 0, 0, 0, time.UTC),
		Info:    BuildInfo{TaskID: "t0"},
		Failed:  true,
	}
	require.NoError(t, failedBuild.Insert())

	buildsChan, errChan := StreamingGetOldBuilds(ctx)
	require.Never(t, func() bool {
		select {
		case <-errChan:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)

	var builds []Build
	require.Eventually(t, func() bool {
		select {
		case b, ok := <-buildsChan:
			if !ok {
				return true
			}
			builds = append(builds, b)
			return false
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
	require.Len(t, builds, 1)
	assert.Equal(t, oldBuild.Id, builds[0].Id)
}

func TestNewBuildId(t *testing.T) {
	result, err := NewBuildId("A", 123)
	require.NoError(t, err)
	assert.Equal(t, "1e7747b3e13274f0bee0de868c8314c9", result)

	result, err = NewBuildId("", -10000)
	require.NoError(t, err)
	assert.Equal(t, "7d2e3a33d801c1ac74f062b41c977104", result)

	result, err = NewBuildId(`{"builder": "builder", "buildNum": "1000"}`, 0)
	require.NoError(t, err)
	assert.Equal(t, "ed39e8e7310193625e521204242e80c4", result)

	result, err = NewBuildId("10", 100)
	require.NoError(t, err)
	assert.Equal(t, "f4088565508a32f3e6ff9205408bcce9", result)

	result, err = NewBuildId("100", 10)
	require.NoError(t, err)
	assert.Equal(t, "b2f7b29a7f76e38abe38fc8145c0cf98", result)
}
