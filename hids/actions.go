package hids

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/0xrawsec/gene/v2/engine"
	"github.com/0xrawsec/golang-utils/crypto/file"
	"github.com/0xrawsec/golang-utils/datastructs"
	"github.com/0xrawsec/golang-utils/fsutil"
	"github.com/0xrawsec/golang-utils/log"
	"github.com/0xrawsec/golang-utils/sync/semaphore"
	"github.com/0xrawsec/golang-win32/win32/dbghelp"
	"github.com/0xrawsec/golang-win32/win32/kernel32"
	"github.com/0xrawsec/whids/event"
	"github.com/0xrawsec/whids/utils"
)

const (
	// Actions
	ActionKill      = "kill"
	ActionBlacklist = "blacklist"
	ActionMemdump   = "memdump"
	ActionFiledump  = "filedump"
	ActionRegdump   = "regdump"
	ActionReport    = "report"
	ActionBrief     = "brief"
)

var (
	AvailableActions = []string{
		ActionKill,
		ActionBlacklist,
		ActionMemdump,
		ActionFiledump,
		ActionRegdump,
		ActionReport,
		ActionBrief,
	}

	filedumpXPaths = []engine.XPath{
		pathSysmonImage,
		pathSysmonParentImage,
		pathSysmonSourceImage,
		pathSysmonImageLoaded,
		pathSysmonTargetFilename,
	}

	sysmonArcFileRe = regexp.MustCompile("(((SHA1|MD5|SHA256|IMPHASH)=)|,)")
)

type ActionHandler struct {
	ctx              context.Context
	hids             *HIDS
	queue            *datastructs.Fifo
	compressionQueue *datastructs.Fifo
	semJobs          semaphore.Semaphore
}

func NewActionHandler(h *HIDS) *ActionHandler {
	return &ActionHandler{h.ctx,
		h,
		&datastructs.Fifo{},
		&datastructs.Fifo{},
		semaphore.New(2)}
}

func (m *ActionHandler) dumpname(src string) string {
	base := strings.Replace(filepath.Base(src), ":", "_ADS_", -1)
	return fmt.Sprintf("%d_%s.bin", time.Now().UnixNano(), base)
}

func (m *ActionHandler) prepare(e *event.EdrEvent, filename string) string {
	id := e.Hash()
	guid := srcGUIDFromEvent(e)
	dumpDir := filepath.Join(m.hids.config.Dump.Dir, guid, id)
	utils.HidsMkdirAll(dumpDir)
	return filepath.Join(dumpDir, filename)
}

func (m *ActionHandler) shouldDump(e *event.EdrEvent) bool {
	guid := srcGUIDFromEvent(e)
	return m.hids.tracker.CheckDumpCountOrInc(guid, m.hids.config.Dump.MaxDumps, m.hids.config.Dump.DumpUntracked)
}

func (m *ActionHandler) writeReader(dst string, reader io.Reader) error {
	compress := m.hids.config.Dump.Compression
	return utils.HidsWriteReader(dst, reader, compress)
}

func (m *ActionHandler) dumpAsJson(path string, i interface{}) (err error) {
	var b []byte

	if b, err = json.Marshal(i); err != nil {
		return
	} else {
		if err = m.writeReader(path, bytes.NewBuffer(b)); err != nil {
			return
		}
	}
	return
}

func (m *ActionHandler) dumpBinFile(e *event.EdrEvent, src string) error {
	return m.dumpFile(src, m.prepare(e, m.dumpname(src)))
}

func (m *ActionHandler) dumpFile(src, dst string) (err error) {
	var sha256 string

	if !fsutil.IsFile(src) || utils.IsPipePath(src) {
		return
	}

	if err = utils.HidsMkdirAll(filepath.Dir(dst)); err != nil {
		return err
	}

	if sha256, err = file.Sha256(src); err != nil {
		return err
	}

	// dump sha256 of file anyway
	utils.HidsWriteData(fmt.Sprintf("%s.sha256", dst), []byte(sha256))
	if !m.hids.filedumped.Contains(sha256) {
		var f *os.File
		log.Debugf("Dumping file: %s->%s", src, dst)
		if f, err = os.Open(src); err != nil {
			return
		}
		if err = m.writeReader(dst, f); err != nil {
			return
		}
		// we mark file dumped
		m.hids.filedumped.Add(sha256)
	}
	return
}

func listFilesFromCommandLine(cmdLine string, cwd string) []string {
	files := make([]string, 0)

	if argv, err := utils.ArgvFromCommandLine(cmdLine); err == nil {
		if len(argv) > 1 {
			for _, arg := range argv[1:] {
				if fsutil.IsFile(arg) && !utils.IsPipePath(arg) {
					files = append(files, arg)
				}
				if cwd != "" {
					// relative to CWD
					relarg := filepath.Join(cwd, arg)
					if fsutil.IsFile(arg) && !utils.IsPipePath(arg) {
						files = append(files, relarg)
					}
				}
			}
		}
	}

	return files
}

func (m *ActionHandler) filedumpSet(e *event.EdrEvent) *datastructs.Set {
	s := datastructs.NewSet()

	if pt := processTrackFromEvent(m.hids, e); !pt.IsZero() {
		s.Add(pt.Image)
		s.Add(pt.ParentImage)
		// parse command line
		for _, f := range listFilesFromCommandLine(pt.CommandLine, pt.CurrentDirectory) {
			s.Add(f)
		}
		// parse parent command line
		for _, f := range listFilesFromCommandLine(pt.ParentCommandLine, pt.ParentCurrentDirectory) {
			s.Add(f)
		}
	}

	if e.Channel() == sysmonChannel {
		switch e.EventID() {
		case SysmonRegSetValue:
			if det, ok := e.GetString(pathSysmonDetails); ok {
				for _, f := range listFilesFromCommandLine(det, "") {
					s.Add(f)
				}
			}
		case SysmonWMIConsumer:
			if dest, ok := e.GetString(pathSysmonDestination); ok {
				for _, f := range listFilesFromCommandLine(dest, "") {
					s.Add(f)
				}
			}
		case SysmonFileDelete:
			archived, ok := e.GetBool(pathSysmonArchived)
			if ok && archived {
				if hashes, ok := e.GetString(pathSysmonHashes); ok {
					if target, ok := e.GetString(pathSysmonTargetFilename); ok {
						fname := fmt.Sprintf("%s%s", sysmonArcFileRe.ReplaceAllString(hashes, ""), filepath.Ext(target))
						path := filepath.Join(m.hids.config.Sysmon.ArchiveDirectory, fname)
						s.Add(path)
					}
				}
			}
		}
	}

	for _, p := range filedumpXPaths {
		if filename, ok := e.GetString(p); ok {
			s.Add(filename)
		}
	}

	return s
}

func (m *ActionHandler) filedump(e *event.EdrEvent) {
	hash := e.Hash()
	for _, i := range m.filedumpSet(e).Slice() {
		filename := i.(string)
		if err := m.dumpBinFile(e, filename); err != nil {
			log.Errorf(`Failed to dump file="%s" event=%s`, filename, hash)
		}
	}
}

func (m *ActionHandler) memdump(e *event.EdrEvent) (err error) {
	hash := e.Hash()
	if pt := processTrackFromEvent(m.hids, e); !pt.IsZero() {
		guid := srcGUIDFromEvent(e)
		pid := int(pt.PID)
		if kernel32.IsPIDRunning(pid) && pid != os.Getpid() && !m.hids.memdumped.Contains(guid) && !m.hids.dumping.Contains(guid) {
			// To avoid dumping the same process twice, possible if two alerts
			// comes from the same GUID in a short period of time
			m.hids.dumping.Add(guid)
			defer m.hids.dumping.Del(guid)

			dumpFilename := fmt.Sprintf("%s_%d_%d.dmp", filepath.Base(pt.Image), pid, time.Now().UnixNano())
			dumpPath := m.prepare(e, dumpFilename)
			if err = dbghelp.FullMemoryMiniDump(pid, dumpPath); err != nil {
				return fmt.Errorf("failed to dump process event=%s pid=%d image=%s: %s", hash, pid, pt.Image, err)
			} else {
				// dump was successfull
				m.hids.memdumped.Add(guid)
				m.compress(dumpPath)
			}
		} else {
			return fmt.Errorf("cannot dump process event=%s pid=%d, process is already terminated", hash, pid)
		}
	} else {
		return fmt.Errorf("cannot dump untracked process event=%s", hash)
	}
	return
}

func (m *ActionHandler) regdump(e *event.EdrEvent) {
	var err error
	var content string

	if e.Channel() == sysmonChannel {
		switch e.EventID() {
		case SysmonRegSetValue:
			if targetObject, ok := e.GetString(pathSysmonTargetObject); ok {
				if details, ok := e.GetString(pathSysmonDetails); ok {
					// We dump only if Details is "Binary Data" since the other kinds can be seen in the raw event
					if details == "Binary Data" {
						dumpPath := m.prepare(e, "reg.txt")
						key, value := filepath.Split(targetObject)
						if content, err = utils.RegQuery(key, value); err != nil {
							log.Errorf("Failed to run reg query: %s", err)
							content = fmt.Sprintf("HIDS error dumping %s: %s", targetObject, err)
						}
						if err = m.writeReader(dumpPath, bytes.NewBufferString(content)); err != nil {
							log.Errorf("Failed to write registry content to file: %s", err)
						}
					}
				}
			}

		}
	}
}

func (m *ActionHandler) suspend_process(e *event.EdrEvent) {
	if pt := processTrackFromEvent(m.hids, e); !pt.IsZero() {
		// additional check not to suspend agent
		if pt.PID != int64(os.Getpid()) {
			// before we kill we suspend the process
			kernel32.SuspendProcess(int(pt.PID))
		}
	}
}

func (m *ActionHandler) kill_process(e *event.EdrEvent) error {
	if pt := processTrackFromEvent(m.hids, e); !pt.IsZero() {
		// additional check not to suspend agent
		if pt.PID != int64(os.Getpid()) {
			if err := pt.TerminateProcess(); err != nil {
				return fmt.Errorf("failed to kill process for event=%s image=%s pid=%d guid=%s", e.Hash(), pt.Image, pt.PID, pt.ProcessGUID)

			}
		}
	}
	return nil
}

func (m *ActionHandler) Queue(e *event.EdrEvent) {
	if !m.hids.IsHIDSEvent(e) && m.hids.config.Endpoint {
		if det := e.GetDetection(); det != nil {
			if det.Actions.Len() > 0 {
				m.queue.Push(e)
			}
		}
	}
}

func (m *ActionHandler) HandleActions(e *event.EdrEvent) {

	det := e.GetDetection()

	if m.shouldDump(e) && !m.hids.IsHIDSEvent(e) && det != nil {
		hash := e.Hash()

		// Test variables
		report := det.Actions.Contains(ActionReport)
		brief := det.Actions.Contains(ActionBrief)
		kill := det.Actions.Contains(ActionKill)

		// handling blacklisting action
		if det.Actions.Contains(ActionBlacklist) {
			if pt := processTrackFromEvent(m.hids, e); !pt.IsZero() {
				// additional check not to blacklist agent
				if int(pt.PID) != os.Getpid() {
					m.hids.tracker.Blacklist(pt.CommandLine)
				}
			}
		}

		if kill {
			// we suspend process before to kill it so that we can
			// memdump it
			m.suspend_process(e)
		}

		// handling report memdumping
		if det.Actions.Contains(ActionMemdump) {
			if err := m.memdump(e); err != nil {
				log.Error(err)
			}
		}

		// we kill the process after we dumped memory
		if kill {
			if err := m.kill_process(e); err != nil {
				log.Error(err)
			}
		}

		// handling report dumping
		if (report || brief) && m.hids.config.Report.EnableReporting {
			if err := m.dumpAsJson(m.prepare(e, "report.json"), m.hids.Report(brief)); err != nil {
				log.Errorf("Failed to dump report for event %s: %s", hash, err)
			}
		}

		// handling filedumping
		if det.Actions.Contains(ActionFiledump) {
			m.filedump(e)
		}

		// handling regdumping
		if det.Actions.Contains(ActionRegdump) {
			m.regdump(e)
		}

		// dumping the event
		if err := m.dumpAsJson(m.prepare(e, "event.json"), e); err != nil {
			log.Errorf("Failed to dump event %s: %s", hash, err)
		}

	}
}

func (m *ActionHandler) compress(path string) {
	if m.hids.config.Dump.Compression {
		m.compressionQueue.Push(path)
	}
}

func (m *ActionHandler) compressionRoutine() {
	go func() {
		for m.ctx.Err() == nil {
			for m.compressionQueue.Len() > 0 {
				if elt := m.compressionQueue.Pop(); elt != nil {
					path := elt.Value.(string)
					if err := utils.GzipFileBestSpeed(path); err != nil {
						log.Errorf(`Failed to compress %s: %s`, path, err)
					}
				}
			}
			time.Sleep(time.Second)
		}
	}()
}

func (m *ActionHandler) Run() {
	go func() {
		for m.ctx.Err() == nil {
			for m.queue.Len() > 0 {
				if elt := m.queue.Pop(); elt != nil {
					evt := elt.Value.(*event.EdrEvent)
					m.semJobs.Acquire()
					go func() {
						defer m.semJobs.Release()
						m.HandleActions(evt)
					}()
				}
			}
			time.Sleep(time.Millisecond * 50)
		}
	}()
	// run compression routine
	m.compressionRoutine()
}
