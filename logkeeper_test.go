package logkeeper

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/evergreen-ci/logkeeper/db"
	"github.com/evergreen-ci/logkeeper/env"
	"github.com/evergreen-ci/logkeeper/model"
	"github.com/mongodb/grip"
	. "github.com/smartystreets/goconvey/convey"
	"github.com/smartystreets/goconvey/convey/reporting"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

func resetDatabase(db *mgo.Database) {
	grip.Error(db.DropDatabase())
}

func init() {
	reporting.QuietMode()
}

func TestLogKeeper(t *testing.T) {
	session, err := mgo.Dial("localhost:27017")
	if err != nil {
		t.Fatal(err)
	}

	env.SetDBName("logkeeper_test")
	if err = env.SetSession(session); err != nil {
		t.Fatal(err)
	}
	db, closer := db.DB()
	defer closer()

	Convey("LogKeeper instance running on testdatabase", t, func() {
		lk := New(Options{MaxRequestSize: 1024 * 1024 * 10})
		router := lk.NewRouter()

		Convey("Call POST /build creates a build with the given builder/buildnum", func() {
			r := newTestRequest(lk, "POST", "/build/", map[string]interface{}{"builder": "poop", "buildnum": 123})
			data := checkEndpointResponse(router, r, http.StatusCreated)
			So(data["id"], ShouldNotBeNil)
			So(data["uri"], ShouldNotBeNil)
			originalId, originalURI := data["id"], data["uri"]

			// Call POST /build again,
			r = newTestRequest(lk, "POST", "/build/", map[string]interface{}{"builder": "poop", "buildnum": 123})
			data = checkEndpointResponse(router, r, http.StatusOK)
			So(data["id"], ShouldEqual, originalId)
			So(data["uri"], ShouldEqual, originalURI)
		})

		Convey("Logkeeper breaks oversize log into pieces", func() {
			// Create build and test
			r := newTestRequest(lk, "POST", "/build", map[string]interface{}{"builder": "myBuilder", "buildnum": 123})
			data := checkEndpointResponse(router, r, http.StatusCreated)
			So(data["id"], ShouldNotBeNil)
			buildId := data["id"].(string)
			r = newTestRequest(lk, "POST", "/build/"+buildId+"/test", map[string]interface{}{"test_filename": "myTestFileName", "command": "myCommand", "phase": "myPhase"})
			data = checkEndpointResponse(router, r, http.StatusCreated)
			So(data["id"], ShouldNotBeNil)
			testId := data["id"].(string)

			// Insert oversize log
			line := strings.Repeat("a", 2097152)
			now := time.Now().Unix()
			r = newTestRequest(lk, "POST", "/build/"+buildId+"/test/"+testId, [][]interface{}{{now, line}, {now, line}, {now, line}})
			data = checkEndpointResponse(router, r, http.StatusCreated)
			So(len(data), ShouldBeGreaterThan, 0)

			// Test should have seq = 2
			test, err := model.FindTestByID(testId)
			So(err, ShouldBeNil)
			So(test.Seq, ShouldEqual, 2)

			// Test should have two logs
			numLogs, err := db.C("logs").Find(bson.M{"test_id": bson.ObjectIdHex(testId)}).Count()
			So(err, ShouldBeNil)
			So(numLogs, ShouldEqual, 2)

			// First log should have two lines and seq=1
			// Second log should have one line and seq=2
			logs := db.C("logs").Find(bson.M{"test_id": bson.ObjectIdHex(testId)}).Sort("seq").Iter()
			log := &model.Log{}
			firstLog := true
			for logs.Next(log) {
				if firstLog {
					So(len(log.Lines), ShouldEqual, 2)
					So(log.Seq, ShouldEqual, 1)
					firstLog = false
				} else {
					So(len(log.Lines), ShouldEqual, 1)
					So(log.Seq, ShouldEqual, 2)
				}
			}

			So(db.DropDatabase(), ShouldBeNil)

			// Create build
			r = newTestRequest(lk, "POST", "/build", map[string]interface{}{"builder": "myBuilder", "buildnum": 123})
			data = checkEndpointResponse(router, r, http.StatusCreated)
			So(data["id"], ShouldNotBeNil)
			buildId = data["id"].(string)

			// Insert oversize global log
			r = newTestRequest(lk, "POST", "/build/"+buildId, [][]interface{}{{now, line}, {now, line}, {now, line}})
			data = checkEndpointResponse(router, r, http.StatusCreated)
			So(len(data), ShouldBeGreaterThan, 0)

			// Build should have seq = 2
			build, err := model.FindBuildById(buildId)
			So(err, ShouldBeNil)
			So(build.Seq, ShouldEqual, 2)

			// Build should have two logs
			numLogs, err = db.C("logs").Find(bson.M{"build_id": buildId}).Count()
			So(err, ShouldBeNil)
			So(numLogs, ShouldEqual, 2)

			// First log should have two lines and seq=1
			// Second log should have one line and seq=2
			logs = db.C("logs").Find(bson.M{"build_id": buildId}).Sort("seq").Iter()
			log = &model.Log{}
			firstLog = true
			for logs.Next(log) {
				if firstLog {
					So(len(log.Lines), ShouldEqual, 2)
					So(log.Seq, ShouldEqual, 1)
					firstLog = false
				} else {
					So(len(log.Lines), ShouldEqual, 1)
					So(log.Seq, ShouldEqual, 2)
				}
			}

			// Inserting oversize log line fails
			line = strings.Repeat("a", 4194305)
			r = newTestRequest(lk, "POST", "/build/"+buildId, [][]interface{}{{now, line}})
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			So(w.Code, ShouldEqual, http.StatusBadRequest)

		})

		Convey("Adding the task id field will correctly insert it in the database", func() {
			// Create build and test
			r := newTestRequest(lk, "POST", "/build", map[string]interface{}{"builder": "myBuilder", "buildnum": 123})
			data := checkEndpointResponse(router, r, http.StatusCreated)
			So(data["id"], ShouldNotBeNil)
			buildId := data["id"].(string)
			r = newTestRequest(lk, "POST", "/build/"+buildId+"/test", map[string]interface{}{"test_filename": "myTestFileName", "command": "myCommand", "phase": "myPhase", "task_id": "abc123"})
			data = checkEndpointResponse(router, r, http.StatusCreated)
			So(data["id"], ShouldNotBeNil)
			testId := data["id"].(string)

			test := &model.Test{}
			err := db.C("tests").Find(bson.M{"_id": bson.ObjectIdHex(testId)}).One(test)
			So(err, ShouldBeNil)
			So(test.Info, ShouldNotBeNil)
			So(test.Info.TaskID, ShouldEqual, "abc123")
		})

		// Clear database
		Reset(func() { resetDatabase(db) })
	})
}

func checkEndpointResponse(router http.Handler, req *http.Request, expectedCode int) map[string]interface{} {
	w := httptest.NewRecorder()
	decoded := map[string]interface{}{}
	router.ServeHTTP(w, req)
	err := json.Unmarshal(w.Body.Bytes(), &decoded)
	So(err, ShouldBeNil)
	So(w.Code, ShouldEqual, expectedCode)
	return decoded
}

func newTestRequest(lk *logKeeper, method, path string, body interface{}) *http.Request {
	req, err := http.NewRequest(method, lk.opts.URL+path, nil)
	if err != nil {
		panic(err)
	}
	jsonbytes, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	req.Body = ioutil.NopCloser(bytes.NewReader(jsonbytes))
	return req
}
