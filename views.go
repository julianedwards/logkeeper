package logkeeper

import (
	"fmt"
	"html/template"
	"net/http"
	"sync"
	"time"

	"github.com/evergreen-ci/logkeeper/env"
	"github.com/evergreen-ci/logkeeper/model"
	"github.com/evergreen-ci/logkeeper/storage"
	"github.com/evergreen-ci/render"
	"github.com/gorilla/mux"
	"github.com/mongodb/amboy"
	"github.com/mongodb/grip"
	"github.com/mongodb/grip/message"
	"github.com/mongodb/grip/recovery"
	"gopkg.in/mgo.v2/bson"
)

const maxLogBytes = 4 * 1024 * 1024 // 4 MB

type Options struct {
	//Base URL to append to relative paths
	URL string

	// Maximum Request Size
	MaxRequestSize int

	// Bucket stores data in offline storage.
	Bucket storage.Bucket
}

type logKeeper struct {
	render *render.Render
	opts   Options
}

type createdResponse struct {
	Id  string `json:"id,omitempty"`
	URI string `json:"uri"`
}

func New(opts Options) *logKeeper {
	render := render.New(render.Options{
		Directory: "templates",
		Funcs: template.FuncMap{
			"MutableVar": func() interface{} {
				return &MutableVar{""}
			},
			"ColorSet": func() *ColorSet {
				return NewColorSet()
			},
			"DateFormat": func(when time.Time, layout string) string {
				return when.Format(layout)
			},
		},
	})

	return &logKeeper{render, opts}
}

type apiError struct {
	Err     string `json:"err"`
	MaxSize int    `json:"max_size,omitempty"`
	code    int
}

type logFetchResponse struct {
	logLines chan *model.LogLineItem
	build    *model.Build
	test     *model.Test
}

func (lk *logKeeper) createBuild(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	if err := lk.checkContentLength(r); err != nil {
		lk.logErrorf(r, "content length limit exceeded for createBuild: %s", err.Err)
		lk.render.WriteJSON(w, err.code, err)
		return
	}

	buildParameters := struct {
		Builder  string `json:"builder"`
		BuildNum int    `json:"buildnum"`
		TaskId   string `json:"task_id"`
		S3       bool   `json:"s3"`
	}{}

	if err := readJSON(r.Body, lk.opts.MaxRequestSize, &buildParameters); err != nil {
		lk.logErrorf(r, "Bad request to createBuild: %s", err.Err)
		lk.render.WriteJSON(w, err.code, err)
		return
	}

	existingBuild, err := model.FindBuildByBuilder(buildParameters.Builder, buildParameters.BuildNum)
	if err != nil {
		lk.logErrorf(r, "Error finding build by builder: %v", err)
		lk.render.WriteJSON(w, http.StatusInternalServerError, apiError{Err: err.Error()})
		return
	}
	if existingBuild != nil {
		existingBuildUri := fmt.Sprintf("%v/build/%v", lk.opts.URL, existingBuild.Id)
		response := createdResponse{existingBuild.Id, existingBuildUri}
		lk.render.WriteJSON(w, http.StatusOK, response)
		return
	}

	newBuildId, err := model.NewBuildId(buildParameters.Builder, buildParameters.BuildNum)
	if err != nil {
		lk.render.WriteJSON(w, http.StatusInternalServerError, apiError{Err: err.Error()})
		return
	}

	newBuild := model.Build{
		Id:       newBuildId,
		Builder:  buildParameters.Builder,
		BuildNum: buildParameters.BuildNum,
		Name:     fmt.Sprintf("%v #%v", buildParameters.Builder, buildParameters.BuildNum),
		Started:  time.Now(),
		Info:     model.BuildInfo{TaskID: buildParameters.TaskId},
		S3:       buildParameters.S3,
	}
	if err = newBuild.Insert(); err != nil {
		lk.logErrorf(r, "Error inserting build object: %v", err)
		lk.render.WriteJSON(w, http.StatusInternalServerError, apiError{Err: err.Error()})
		return
	}

	if buildParameters.S3 {
		if err := lk.opts.Bucket.UploadBuildMetadata(r.Context(), newBuild); err != nil {
			lk.logErrorf(r, "writing build metadata: %v", err)
			lk.render.WriteJSON(w, http.StatusInternalServerError, apiError{Err: err.Error()})
			return
		}
	}

	newBuildUri := fmt.Sprintf("%v/build/%v", lk.opts.URL, newBuildId)

	response := createdResponse{newBuildId, newBuildUri}
	lk.render.WriteJSON(w, http.StatusCreated, response)
}

func (lk *logKeeper) createTest(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	if err := lk.checkContentLength(r); err != nil {
		lk.logErrorf(r, "content length limit exceeded for createTest: %s", err.Err)
		lk.render.WriteJSON(w, err.code, err)
		return
	}

	vars := mux.Vars(r)
	buildID := vars["build_id"]

	build, err := model.FindBuildById(buildID)
	if err != nil {
		lk.logErrorf(r, "error finding build: %v", err)
		lk.render.WriteJSON(w, http.StatusInternalServerError, apiError{Err: err.Error()})
		return
	}
	if build == nil {
		lk.render.WriteJSON(w, http.StatusNotFound, apiError{Err: "creating test: build not found"})
		return
	}

	testParams := struct {
		TestFilename string `json:"test_filename"`
		Command      string `json:"command"`
		Phase        string `json:"phase"`
		TaskId       string `json:"task_id"`
	}{}

	if err := readJSON(r.Body, lk.opts.MaxRequestSize, &testParams); err != nil {
		lk.logErrorf(r, "Bad request to createTest: %s", err.Err)
		lk.render.WriteJSON(w, err.code, err)
		return
	}

	newTest := model.Test{
		Id:        bson.NewObjectId(),
		BuildId:   build.Id,
		BuildName: build.Name,
		Name:      testParams.TestFilename,
		Command:   testParams.Command,
		Started:   time.Now(),
		Phase:     testParams.Phase,
		Info:      model.TestInfo{TaskID: testParams.TaskId},
	}
	if err := newTest.Insert(); err != nil {
		lk.logErrorf(r, "Error inserting test: %v", err)
		lk.render.WriteJSON(w, http.StatusInternalServerError, apiError{Err: err.Error()})
		return
	}

	if build.S3 {
		if err := lk.opts.Bucket.UploadTestMetadata(r.Context(), newTest); err != nil {
			lk.logErrorf(r, "writing test metadata: %v", err)
			lk.render.WriteJSON(w, http.StatusInternalServerError, apiError{Err: err.Error()})
			return
		}
	}

	testUri := fmt.Sprintf("%s/build/%s/test/%s", lk.opts.URL, build.Id, newTest.Id.Hex())
	lk.render.WriteJSON(w, http.StatusCreated, createdResponse{newTest.Id.Hex(), testUri})
}

func (lk *logKeeper) appendLog(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	if err := lk.checkContentLength(r); err != nil {
		lk.logWarningf(r, "content length limit exceeded for appendLog: %s", err.Err)
		lk.render.WriteJSON(w, err.code, err)
		return
	}

	vars := mux.Vars(r)
	buildID := vars["build_id"]

	build, err := model.FindBuildById(buildID)
	if err != nil || build == nil {
		lk.render.WriteJSON(w, http.StatusNotFound, apiError{Err: "appending log: build not found"})
		return
	}

	testID := vars["test_id"]
	test, err := model.FindTestByID(testID)
	if err != nil || test == nil {
		lk.render.WriteJSON(w, http.StatusNotFound, apiError{Err: "test not found"})
		return
	}

	var lines []model.LogLine
	if err := readJSON(r.Body, lk.opts.MaxRequestSize, &lines); err != nil {
		lk.logErrorf(r, "Bad request to appendLog: %s", err.Err)
		lk.render.WriteJSON(w, err.code, err)
		return
	}

	if len(lines) == 0 {
		// no need to insert anything, so stop here
		lk.render.WriteJSON(w, http.StatusOK, "")
		return
	}

	chunks, err := model.GroupLines(lines, maxLogBytes)
	if err != nil {
		lk.logErrorf(r, "unmarshaling log lines: %v", err)
		lk.render.WriteJSON(w, http.StatusBadRequest, apiError{Err: err.Error()})
		return
	}

	if err = test.IncrementSequence(len(chunks)); err != nil {
		lk.logErrorf(r, "Error updating tests: %v", err)
		lk.render.WriteJSON(w, http.StatusInternalServerError, apiError{Err: err.Error()})
		return
	}

	if err = model.InsertLogChunks(build.Id, &test.Id, test.Seq, chunks); err != nil {
		lk.logErrorf(r, "Error inserting logs: %v", err)
		lk.render.WriteJSON(w, http.StatusInternalServerError, apiError{Err: err.Error()})
		return
	}

	if build.S3 {
		if err := lk.opts.Bucket.InsertLogChunks(r.Context(), build.Id, test.Id.Hex(), chunks); err != nil {
			lk.logErrorf(r, "appending S3 logs: %v", err)
			lk.render.WriteJSON(w, http.StatusInternalServerError, apiError{Err: err.Error()})
			return
		}
	}

	testUrl := fmt.Sprintf("%s/build/%s/test/%s", lk.opts.URL, build.Id, test.Id.Hex())
	lk.render.WriteJSON(w, http.StatusCreated, createdResponse{"", testUrl})
}

func (lk *logKeeper) appendGlobalLog(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	if err := lk.checkContentLength(r); err != nil {
		lk.logWarningf(r, "content length limit exceeded for appendGlobalLog: %s", err.Err)
		lk.render.WriteJSON(w, err.code, err)
		return
	}

	vars := mux.Vars(r)
	buildID := vars["build_id"]

	build, err := model.FindBuildById(buildID)
	if err != nil {
		lk.logErrorf(r, "Error finding builds entry: %v", err)
		lk.render.WriteJSON(w, http.StatusInternalServerError, apiError{Err: "finding builds in append global log:" + err.Error()})
		return
	}
	if build == nil {
		lk.render.WriteJSON(w, http.StatusNotFound, apiError{Err: "append global log: build not found"})
		return
	}

	var lines []model.LogLine
	if err := readJSON(r.Body, lk.opts.MaxRequestSize, &lines); err != nil {
		lk.logErrorf(r, "Bad request to appendGlobalLog: %s", err.Err)
		lk.render.WriteJSON(w, err.code, err)
		return
	}

	if len(lines) == 0 {
		// no need to insert anything, so stop here
		lk.render.WriteJSON(w, http.StatusOK, "")
		return
	}

	chunks, err := model.GroupLines(lines, maxLogBytes)
	if err != nil {
		lk.logErrorf(r, "unmarshaling log lines: %v", err)
		lk.render.WriteJSON(w, http.StatusBadRequest, apiError{Err: err.Error()})
		return
	}

	if err = build.IncrementSequence(len(chunks)); err != nil {
		lk.logErrorf(r, "Error updating tests: %v", err)
		lk.render.WriteJSON(w, http.StatusInternalServerError, apiError{Err: err.Error()})
		return
	}

	if err = model.InsertLogChunks(build.Id, nil, build.Seq, chunks); err != nil {
		lk.logErrorf(r, "Error inserting logs: %v", err)
		lk.render.WriteJSON(w, http.StatusInternalServerError, apiError{Err: err.Error()})
		return
	}

	if build.S3 {
		if err := lk.opts.Bucket.InsertLogChunks(r.Context(), build.Id, "", chunks); err != nil {
			lk.logErrorf(r, "appending S3 logs: %v", err)
			lk.render.WriteJSON(w, http.StatusInternalServerError, apiError{Err: err.Error()})
			return
		}
	}

	testUrl := fmt.Sprintf("%s/build/%s/", lk.opts.URL, build.Id)
	lk.render.WriteJSON(w, http.StatusCreated, createdResponse{"", testUrl})
}

func (lk *logKeeper) viewBuildByIdInS3(r *http.Request, buildID string) (*model.Build, []model.Test, *apiError) {
	var wg sync.WaitGroup
	wg.Add(2)

	var (
		build    *model.Build
		buildErr error
	)
	go func() {
		defer recovery.LogStackTraceAndContinue("fetching build from s3 for test id")
		defer wg.Done()
		build, buildErr = lk.opts.Bucket.FindBuildByID(r.Context(), buildID)
	}()

	var (
		tests    []model.Test
		testsErr error
	)
	go func() {
		defer recovery.LogStackTraceAndContinue("fetching build from s3 for test id")
		defer wg.Done()
		tests, testsErr = lk.opts.Bucket.FindTestsForBuild(r.Context(), buildID)
	}()

	wg.Wait()

	if buildErr != nil {
		lk.logErrorf(r, "Error finding build '%s': %v", buildID, buildErr)
		return nil, nil, &apiError{Err: fmt.Sprintf("failed to find build '%s' in S3: %s", buildID, buildErr.Error()), code: http.StatusInternalServerError}
	}
	if build == nil {
		return nil, nil, &apiError{Err: fmt.Sprintf("build '%s' not found in S3", buildID), code: http.StatusNotFound}
	}
	if testsErr != nil {
		lk.logErrorf(r, "Error finding tests for build '%s': %v", buildID, testsErr)
		return nil, nil, &apiError{Err: testsErr.Error(), code: http.StatusInternalServerError}
	}
	return build, tests, nil
}

func (lk *logKeeper) viewBuildByIdInDatabase(r *http.Request, buildID string) (*model.Build, []model.Test, *apiError) {
	build, err := model.FindBuildById(buildID)
	if err != nil {
		lk.logErrorf(r, "Error finding build '%s': %v", buildID, err)
		return nil, nil, &apiError{Err: fmt.Sprintf("failed to find build '%s': %s", buildID, err.Error()), code: http.StatusInternalServerError}
	}
	if build == nil {
		return nil, nil, &apiError{Err: fmt.Sprintf("build '%s' not found", buildID), code: http.StatusNotFound}
	}

	tests, err := model.FindTestsForBuild(buildID)
	if err != nil {
		lk.logErrorf(r, "Error finding tests for build '%s': %v", buildID, err)
		return nil, nil, &apiError{Err: err.Error(), code: http.StatusInternalServerError}
	}
	return build, tests, nil
}

func (lk *logKeeper) viewBuildById(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Access-Control-Allow-Origin", "*")
	defer r.Body.Close()

	vars := mux.Vars(r)
	buildID := vars["build_id"]

	var (
		build      *model.Build
		tests      []model.Test
		fetchError *apiError
	)
	if len(r.FormValue("s3")) > 0 {
		build, tests, fetchError = lk.viewBuildByIdInS3(r, buildID)
	} else {
		build, tests, fetchError = lk.viewBuildByIdInDatabase(r, buildID)
	}

	if fetchError != nil {
		lk.render.WriteJSON(w, fetchError.code, *fetchError)
		return
	}

	lk.render.WriteHTML(w, http.StatusOK, struct {
		Build *model.Build
		Tests []model.Test
	}{build, tests}, "base", "build.html")
}

func (lk *logKeeper) viewAllLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Access-Control-Allow-Origin", "*")
	defer r.Body.Close()

	vars := mux.Vars(r)
	buildID := vars["build_id"]

	if lobsterRedirect(r) {
		http.Redirect(w, r, fmt.Sprintf("/lobster/build/%s/all", buildID), http.StatusFound)
		return
	}

	build, err := model.FindBuildById(buildID)
	if err != nil || build == nil {
		lk.render.WriteJSON(w, http.StatusNotFound, apiError{Err: "view all logs: build not found"})
		return
	}

	logsChannel, err := model.AllLogs(build.Id)
	if err != nil {
		lk.logErrorf(r, "Error finding logs: %v", err)
		lk.render.WriteJSON(w, http.StatusInternalServerError, apiError{Err: err.Error()})
		return
	}

	if len(r.FormValue("raw")) > 0 || r.Header.Get("Accept") == "text/plain" {
		for line := range logsChannel {
			_, err = w.Write([]byte(line.Data + "\n"))
			if err != nil {
				return
			}
		}
		return
	} else {
		err = lk.render.StreamHTML(w, http.StatusOK, struct {
			LogLines chan *model.LogLineItem
			BuildId  string
			Builder  string
			TestId   string
			TestName string
			Info     model.BuildInfo
		}{logsChannel, build.Id, build.Builder, "", "All logs", build.Info}, "base", "test.html")
		if err != nil {
			lk.logErrorf(r, "Error rendering template: %v", err)
		}
	}
}

func (lk *logKeeper) viewTestInDatabase(r *http.Request, buildID string, testID string) (*logFetchResponse, *apiError) {
	build, err := model.FindBuildById(buildID)
	if err != nil || build == nil {
		return nil, &apiError{Err: "view test by id: build not found", code: http.StatusNotFound}
	}

	test, err := model.FindTestByID(testID)
	if err != nil || test == nil {
		return nil, &apiError{Err: "test not found"}
	}

	logsChan, err := model.MergedTestLogs(test)
	if err != nil {
		lk.logErrorf(r, "Error finding global logs during test: %v", err)
		return nil, &apiError{Err: err.Error(), code: http.StatusInternalServerError}
	}

	result := logFetchResponse{
		logLines: logsChan,
		build:    build,
		test:     test,
	}
	return &result, nil
}

func (lk *logKeeper) viewTestInS3(r *http.Request, buildID string, testID string) (*logFetchResponse, *apiError) {
	var build *model.Build
	var buildErr error
	wg := sync.WaitGroup{}

	wg.Add(3)
	go func() {
		defer recovery.LogStackTraceAndContinue("fetching build from s3 for test id")
		defer wg.Done()
		build, buildErr = lk.opts.Bucket.FindBuildByID(r.Context(), buildID)
	}()

	var test *model.Test
	var testErr error
	go func() {
		defer recovery.LogStackTraceAndContinue("fetching test from s3 for test id")
		defer wg.Done()
		test, testErr = lk.opts.Bucket.FindTestByID(r.Context(), buildID, testID)
	}()

	var logsChan chan *model.LogLineItem
	var logsChanErr error
	go func() {
		defer recovery.LogStackTraceAndContinue("fetching log lines from s3 for test id")
		defer wg.Done()
		logsChan, logsChanErr = lk.opts.Bucket.GetTestLogLines(r.Context(), buildID, testID)
	}()

	wg.Wait()

	if buildErr != nil {
		lk.logErrorf(r, "error fetching build: %v", buildErr)
		return nil, &apiError{Err: "error fetching build", code: http.StatusInternalServerError}
	}
	if build == nil {
		return nil, &apiError{Err: fmt.Sprintf("no matching build found for %s", buildID), code: http.StatusNotFound}
	}

	if testErr != nil {
		lk.logErrorf(r, "error fetching test %v", testErr)
		return nil, &apiError{Err: "error fetching test", code: http.StatusInternalServerError}
	}
	if test == nil {
		return nil, &apiError{Err: fmt.Sprintf("no matching test found for build:%s, test:%s", buildID, testID), code: http.StatusNotFound}
	}

	if logsChanErr != nil {
		lk.logErrorf(r, "Error finding logs during test: %v", logsChanErr)
		return nil, &apiError{Err: logsChanErr.Error(), code: http.StatusInternalServerError}
	}

	result := logFetchResponse{
		logLines: logsChan,
		build:    build,
		test:     test,
	}
	return &result, nil
}

func (lk *logKeeper) viewTestByBuildIdTestId(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Access-Control-Allow-Origin", "*")
	defer r.Body.Close()

	vars := mux.Vars(r)
	buildID := vars["build_id"]
	testID := vars["test_id"]

	if lobsterRedirect(r) {
		http.Redirect(w, r, fmt.Sprintf("/lobster/build/%s/test/%s", buildID, testID), http.StatusFound)
		return
	}
	var result *logFetchResponse
	var fetchError *apiError
	if len(r.FormValue("s3")) > 0 {
		result, fetchError = lk.viewTestInS3(r, buildID, testID)
	} else {
		result, fetchError = lk.viewTestInDatabase(r, buildID, testID)
	}
	if fetchError != nil {
		lk.render.WriteJSON(w, fetchError.code, *fetchError)
		return
	}
	logsChan := result.logLines
	build := result.build
	test := result.test
	if len(r.FormValue("raw")) > 0 || r.Header.Get("Accept") == "text/plain" {
		emptyLog := true
		for line := range logsChan {
			emptyLog = false
			_, err := w.Write([]byte(line.Data + "\n"))
			if err != nil {
				lk.render.WriteJSON(w, http.StatusInternalServerError,
					apiError{Err: err.Error()})
				return
			}
		}
		if emptyLog {
			lk.render.WriteJSON(w, http.StatusOK, nil)
		}
	} else {
		err := lk.render.StreamHTML(w, http.StatusOK, struct {
			LogLines chan *model.LogLineItem
			BuildId  string
			Builder  string
			TestId   string
			TestName string
			Info     model.TestInfo
		}{logsChan, build.Id, build.Builder, test.Id.Hex(), test.Name, test.Info}, "base", "test.html")
		// If there was an error, it won't show up in the UI since it's being streamed, so log it here
		// instead
		if err != nil {
			lk.logErrorf(r, "Error rendering template: %v", err)
		}
	}
}

func lobsterRedirect(r *http.Request) bool {
	return len(r.FormValue("html")) == 0 && len(r.FormValue("raw")) == 0 && r.Header.Get("Accept") != "text/plain"
}

func (lk *logKeeper) viewInLobster(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Access-Control-Allow-Origin", "*")
	err := lk.render.StreamHTML(w, http.StatusOK, nil, "base", "lobster/build/index.html")
	if err != nil {
		lk.logErrorf(r, "Error rendering template: %v", err)
	}
}

func (lk *logKeeper) logErrorf(r *http.Request, format string, v ...interface{}) {
	err := fmt.Sprintf(format, v...)
	grip.Error(message.Fields{
		"request": getCtxRequestId(r),
		"error":   err,
	})
}

func (lk *logKeeper) logWarningf(r *http.Request, format string, v ...interface{}) {
	err := fmt.Sprintf(format, v...)
	grip.Warning(message.Fields{
		"request": getCtxRequestId(r),
		"error":   err,
	})
}

func (lk *logKeeper) checkAppHealth(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	resp := struct {
		Err             string           `json:"err"`
		MaxRequestSize  int              `json:"maxRequestSize"`
		DB              bool             `json:"db"`
		Build           string           `json:"build_id"`
		BatchSize       int              `json:"batch_size"`
		NumWorkers      int              `json:"workers"`
		DurationSeconds float64          `json:"dur_secs"`
		CleanupStatus   amboy.QueueStats `json:"cleanup_queue_stats"`
	}{
		Build:           BuildRevision,
		MaxRequestSize:  lk.opts.MaxRequestSize,
		BatchSize:       CleanupBatchSize,
		NumWorkers:      AmboyWorkers,
		DurationSeconds: AmboyInterval.Seconds(),
		CleanupStatus:   env.CleanupQueue().Stats(r.Context()),
	}

	resp.DB = true
	lk.render.WriteJSON(w, http.StatusOK, &resp)
}

func (lk *logKeeper) NewRouter() *mux.Router {
	r := mux.NewRouter().StrictSlash(false)

	//write methods
	r.Path("/build/").Methods("POST").HandlerFunc(lk.createBuild)
	r.Path("/build").Methods("POST").HandlerFunc(lk.createBuild)
	r.Path("/build/{build_id}/test/").Methods("POST").HandlerFunc(lk.createTest)
	r.Path("/build/{build_id}/test").Methods("POST").HandlerFunc(lk.createTest)
	r.Path("/build/{build_id}/test/{test_id}/").Methods("POST").HandlerFunc(lk.appendLog)
	r.Path("/build/{build_id}/test/{test_id}").Methods("POST").HandlerFunc(lk.appendLog)
	r.Path("/build/{build_id}/").Methods("POST").HandlerFunc(lk.appendGlobalLog)
	r.Path("/build/{build_id}").Methods("POST").HandlerFunc(lk.appendGlobalLog)

	//read methods
	r.StrictSlash(true).Path("/build/{build_id}").Methods("GET").HandlerFunc(lk.viewBuildById)
	r.StrictSlash(true).Path("/build/{build_id}/all").Methods("GET").HandlerFunc(lk.viewAllLogs)
	r.StrictSlash(true).Path("/build/{build_id}/test/{test_id}").Methods("GET").HandlerFunc(lk.viewTestByBuildIdTestId)
	r.PathPrefix("/lobster").Methods("GET").HandlerFunc(lk.viewInLobster)
	//r.Path("/{builder}/builds/{buildnum:[0-9]+}/").HandlerFunc(viewBuild)
	//r.Path("/{builder}/builds/{buildnum}/test/{test_phase}/{test_name}").HandlerFunc(app.MakeHandler(Name("view_test")))
	r.Path("/status").Methods("GET").HandlerFunc(lk.checkAppHealth)

	return r
}
