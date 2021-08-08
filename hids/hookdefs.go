package hids

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/0xrawsec/gene/engine"
	"github.com/0xrawsec/golang-utils/crypto/file"

	"github.com/0xrawsec/golang-evtx/evtx"
	"github.com/0xrawsec/golang-utils/fsutil"
	"github.com/0xrawsec/golang-utils/log"
	"github.com/0xrawsec/golang-win32/win32"
	"github.com/0xrawsec/golang-win32/win32/advapi32"
	"github.com/0xrawsec/golang-win32/win32/dbghelp"
	"github.com/0xrawsec/golang-win32/win32/kernel32"
	"github.com/0xrawsec/whids/utils"
)

////////////////////////////////// Hooks //////////////////////////////////

const (
	// Empty GUID
	nullGUID = "{00000000-0000-0000-0000-000000000000}"
)

const (
	// Actions
	ActionKill      = "kill"
	ActionBlacklist = "blacklist"
	ActionMemdump   = "memdump"
	ActionFiledump  = "filedump"
	ActionRegdump   = "regdump"
	ActionReport    = "report"
)

var (
	selfPath, _ = filepath.Abs(os.Args[0])
)

var (
	compressionChannel = make(chan string)

	errServiceResolution = fmt.Errorf("error resolving service name")
)

// hook applying on Sysmon events containing image information and
// adding a new field containing the image size
func hookSetImageSize(h *HIDS, e *evtx.GoEvtxMap) {
	var path *evtx.GoEvtxPath
	var modpath *evtx.GoEvtxPath
	switch e.EventID() {
	case SysmonProcessCreate:
		path = &pathSysmonImage
		modpath = &pathImSize
	default:
		path = &pathSysmonImageLoaded
		modpath = &pathImLoadedSize
	}
	if image, err := e.GetString(path); err == nil {
		if fsutil.IsFile(image) {
			if stat, err := os.Stat(image); err == nil {
				e.Set(modpath, toString(stat.Size()))
			}
		}
	}
}

func hookImageLoad(h *HIDS, e *evtx.GoEvtxMap) {
	e.Set(&pathImageLoadParentImage, "?")
	e.Set(&pathImageLoadParentCommandLine, "?")
	if guid, err := e.GetString(&pathSysmonProcessGUID); err == nil {
		if track := h.processTracker.GetByGuid(guid); track != nil {
			if image, err := e.GetString(&pathSysmonImage); err == nil {
				// make sure that we are taking signature of the image and not
				// one of its DLL
				if image == track.Image {
					if signed, err := e.GetBool(&pathSysmonSigned); err == nil {
						track.Signed = signed
					}
					if signature, err := e.GetString(&pathSysmonSignature); err == nil {
						track.Signature = signature
					}
					if sigStatus, err := e.GetString(&pathSysmonSignatureStatus); err == nil {
						track.SignatureStatus = sigStatus
					}
				}
			}
			e.Set(&pathImageLoadParentImage, track.ParentImage)
			e.Set(&pathImageLoadParentCommandLine, track.ParentCommandLine)
		}
	}
}

// hooks Windows DNS client logs and maintain a domain name resolution table
/*func hookDNS(h *HIDS, e *evtx.GoEvtxMap) {
	if qresults, err := e.GetString(&pathQueryResults); err == nil {
		if qresults != "" && qresults != "-" {
			records := strings.Split(qresults, ";")
			for _, r := range records {
				// check if it is a valid IP
				if net.ParseIP(r) != nil {
					if qvalue, err := e.GetString(&pathQueryName); err == nil {
						dnsResolution[r] = qvalue
					}
				}
			}
		}
	}
}*/

// hook tracking processes
func hookTrack(h *HIDS, e *evtx.GoEvtxMap) {
	switch e.EventID() {
	case SysmonProcessCreate:
		// Default values
		e.Set(&pathAncestors, "?")
		e.Set(&pathParentUser, "?")
		e.Set(&pathParentIntegrityLevel, "?")
		e.Set(&pathParentServices, "?")
		// We need to be sure that process termination is enabled
		// before initiating process tracking not to fill up memory
		// with structures that will never be freed
		if h.flagProcTermEn || !h.bootCompleted {
			if guid, err := e.GetString(&pathSysmonProcessGUID); err == nil {
				if pid, err := e.GetInt(&pathSysmonProcessId); err == nil {
					if image, err := e.GetString(&pathSysmonImage); err == nil {
						// Boot sequence is completed when LogonUI.exe is strarted
						if strings.EqualFold(image, "C:\\Windows\\System32\\LogonUI.exe") {
							log.Infof("Boot sequence completed")
							h.bootCompleted = true
						}
						if commandLine, err := e.GetString(&pathSysmonCommandLine); err == nil {
							if pCommandLine, err := e.GetString(&pathSysmonParentCommandLine); err == nil {
								if pImage, err := e.GetString(&pathSysmonParentImage); err == nil {
									if pguid, err := e.GetString(&pathSysmonParentProcessGUID); err == nil {
										if user, err := e.GetString(&pathSysmonUser); err == nil {
											if il, err := e.GetString(&pathSysmonIntegrityLevel); err == nil {
												if cd, err := e.GetString(&pathSysmonCurrentDirectory); err == nil {
													if hashes, err := e.GetString(&pathSysmonHashes); err == nil {

														track := NewProcessTrack(image, pguid, guid, pid)
														track.ParentImage = pImage
														track.CommandLine = commandLine
														track.ParentCommandLine = pCommandLine
														track.CurrentDirectory = cd
														track.User = user
														track.IntegrityLevel = il
														track.SetHashes(hashes)

														if parent := h.processTracker.GetByGuid(pguid); parent != nil {
															track.Ancestors = append(parent.Ancestors, parent.Image)
															track.ParentUser = parent.User
															track.ParentIntegrityLevel = parent.IntegrityLevel
															track.ParentServices = parent.Services
															track.ParentCurrentDirectory = parent.CurrentDirectory
														} else {
															// For processes created by System
															if pimage, err := e.GetString(&pathSysmonParentImage); err == nil {
																track.Ancestors = append(track.Ancestors, pimage)
															}
														}
														h.processTracker.Add(track)
														e.Set(&pathAncestors, strings.Join(track.Ancestors, "|"))
														if track.ParentUser != "" {
															e.Set(&pathParentUser, track.ParentUser)
														}
														if track.ParentIntegrityLevel != "" {
															e.Set(&pathParentIntegrityLevel, track.ParentIntegrityLevel)
														}
														if track.ParentServices != "" {
															e.Set(&pathParentServices, track.ParentServices)
														}
													}
												}
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
	case SysmonDriverLoad:
		d := DriverInfo{"?", nil, "?", "?", "?", false}
		if hashes, err := e.GetString(&pathSysmonHashes); err == nil {
			d.SetHashes(hashes)
		}
		if imloaded, err := e.GetString(&pathSysmonImageLoaded); err == nil {
			d.Image = imloaded
		}
		if signature, err := e.GetString(&pathSysmonSignature); err == nil {
			d.Signature = signature
		}
		if sigstatus, err := e.GetString(&pathSysmonSignatureStatus); err == nil {
			d.SignatureStatus = sigstatus
		}
		if signed, err := e.GetBool(&pathSysmonSigned); err == nil {
			d.Signed = signed
		}
		h.processTracker.Drivers = append(h.processTracker.Drivers, d)
	}
}

// hook managing statistics about some events
func hookStats(h *HIDS, e *evtx.GoEvtxMap) {
	// We do not store stats if process termination is not enabled
	if h.flagProcTermEn {
		if guid, err := e.GetString(&pathSysmonProcessGUID); err == nil {
			if pt := h.processTracker.GetByGuid(guid); pt != nil {
				switch e.EventID() {
				case SysmonProcessCreate:
					pt.Stats.CreateProcessCount++

				case SysmonNetworkConnect:
					if ip, err := e.GetString(&pathSysmonDestIP); err == nil {
						if port, err := e.GetInt(&pathSysmonDestPort); err == nil {
							if ts, err := e.GetString(&pathSysmonUtcTime); err == nil {
								pt.Stats.UpdateCon(ts, ip, uint16(port))
							}
						}
					}
				case SysmonDNSQuery:
					if ts, err := e.GetString(&pathSysmonUtcTime); err == nil {
						if qvalue, err := e.GetString(&pathQueryName); err == nil {
							if qresults, err := e.GetString(&pathQueryResults); err == nil {
								if qresults != "" && qresults != "-" {
									records := strings.Split(qresults, ";")
									for _, r := range records {
										// check if it is a valid IP
										if net.ParseIP(r) != nil {
											pt.Stats.UpdateNetResolve(ts, r, qvalue)
										}
									}
								}
							}
						}
					}
				case SysmonFileCreate:
					now := time.Now()

					// Set new fields
					e.Set(&pathFileCount, "?")
					e.Set(&pathFileCountByExt, "?")
					e.Set(&pathFileExtension, "?")

					if pt.Stats.Files.TimeFirstFileCreated.IsZero() {
						pt.Stats.Files.TimeFirstFileCreated = now
					}

					if target, err := e.GetString(&pathSysmonTargetFilename); err == nil {
						ext := filepath.Ext(target)
						pt.Stats.Files.CountFilesCreatedByExt[ext]++
						// Setting file count by extension
						e.Set(&pathFileCountByExt, toString(pt.Stats.Files.CountFilesCreatedByExt[ext]))
						// Setting file extension
						e.Set(&pathFileExtension, ext)
					}
					pt.Stats.Files.CountFilesCreated++
					// Setting total file count
					e.Set(&pathFileCount, toString(pt.Stats.Files.CountFilesCreated))
					// Setting frequency
					freq := now.Sub(pt.Stats.Files.TimeFirstFileCreated)
					if freq != 0 {
						eps := pt.Stats.Files.CountFilesCreated * int64(math.Pow10(9)) / freq.Nanoseconds()
						e.Set(&pathFileFrequency, toString(int64(eps)))
					} else {
						e.Set(&pathFileFrequency, toString(0))
					}
					// Finally set last event timestamp
					pt.Stats.Files.TimeLastFileCreated = now

				case SysmonFileDelete, SysmonFileDeleteDetected:
					now := time.Now()

					// Set new fields
					e.Set(&pathFileCount, "?")
					e.Set(&pathFileCountByExt, "?")
					e.Set(&pathFileExtension, "?")

					if pt.Stats.Files.TimeFirstFileDeleted.IsZero() {
						pt.Stats.Files.TimeFirstFileDeleted = now
					}

					if target, err := e.GetString(&pathSysmonTargetFilename); err == nil {
						ext := filepath.Ext(target)
						pt.Stats.Files.CountFilesDeletedByExt[ext]++
						// Setting file count by extension
						e.Set(&pathFileCountByExt, toString(pt.Stats.Files.CountFilesDeletedByExt[ext]))
						// Setting file extension
						e.Set(&pathFileExtension, ext)
					}
					pt.Stats.Files.CountFilesDeleted++
					// Setting total file count
					e.Set(&pathFileCount, toString(pt.Stats.Files.CountFilesDeleted))

					// Setting frequency
					freq := now.Sub(pt.Stats.Files.TimeFirstFileDeleted)
					if freq != 0 {
						eps := pt.Stats.Files.CountFilesDeleted * int64(math.Pow10(9)) / freq.Nanoseconds()
						e.Set(&pathFileFrequency, toString(int64(eps)))
					} else {
						e.Set(&pathFileFrequency, toString(0))
					}

					// Finally set last event timestamp
					pt.Stats.Files.TimeLastFileDeleted = time.Now()
				}
			}
		}
	}
}

func hookUpdateGeneScore(h *HIDS, e *evtx.GoEvtxMap) {
	if h.IsHIDSEvent(e) {
		return
	}

	if t := processTrackFromEvent(h, e); t != nil {
		if i, err := e.Get(&engine.SignaturePath); err == nil {
			t.GeneScore.UpdateCriticality(int64(getCriticality(e)))
			if signatures, ok := (*i).([]string); ok {
				t.GeneScore.UpdateSignature(signatures)
			}
		}
	}
}

func hookHandleActions(h *HIDS, e *evtx.GoEvtxMap) {
	var kill, memdump bool

	// We have to check that if we are handling one of
	// our event and we don't want to kill ourself
	if h.IsHIDSEvent(e) {
		return
	}

	// the only requirement to be able to handle action
	// is to have a process guuid
	if uuid := srcGUIDFromEvent(e); uuid != nullGUID {
		if i, err := e.Get(&engine.ActionsPath); err == nil {
			if actions, ok := (*i).([]string); ok {
				for _, action := range actions {
					switch action {
					case ActionKill:
						kill = true
						if pt := processTrackFromEvent(h, e); pt != nil {
							// additional check not to suspend agent
							if pt.PID != int64(os.Getpid()) {
								// before we kill we suspend the process
								kernel32.SuspendProcess(int(pt.PID))
							}
						}
					case ActionBlacklist:
						if pt := processTrackFromEvent(h, e); pt != nil {
							// additional check not to blacklist agent
							if int(pt.PID) != os.Getpid() {
								h.processTracker.Blacklist(pt.CommandLine)
							}
						}
					case ActionMemdump:
						memdump = true
						dumpProcessRtn(h, e)
					case ActionRegdump:
						dumpRegistryRtn(h, e)
					case ActionFiledump:
						dumpFilesRtn(h, e)
					case ActionReport:
						dumpReportRtn(h, e)
					default:
						log.Errorf("Cannot handle %s action as it is unknown", action)
					}
				}
			}

			// handle kill operation after the other actions
			if kill {
				if pt := processTrackFromEvent(h, e); pt != nil {
					if pt.PID != int64(os.Getpid()) {
						if memdump {
							// Wait we finish dumping before killing the process
							go func() {
								guid := pt.ProcessGUID
								for i := 0; i < 60 && !h.memdumped.Contains(guid); i++ {
									time.Sleep(1 * time.Second)
								}
								if err := pt.TerminateProcess(); err != nil {
									log.Errorf("Failed to terminate process PID=%d GUID=%s", pt.PID, pt.ProcessGUID)
								}
							}()
						} else if err := pt.TerminateProcess(); err != nil {
							log.Errorf("Failed to terminate process PID=%d GUID=%s", pt.PID, pt.ProcessGUID)
						}
					}
				}
			}
		}
	} else {
		log.Errorf("Failed to handle actions for event (channel: %s, id: %d): no process GUID available", e.Channel(), e.EventID())
	}
}

// hook terminating previously blacklisted processes (according to their CommandLine)
func hookTerminator(h *HIDS, e *evtx.GoEvtxMap) {
	if e.EventID() == SysmonProcessCreate {
		if commandLine, err := e.GetString(&pathSysmonCommandLine); err == nil {
			if pid, err := e.GetInt(&pathSysmonProcessId); err == nil {
				if h.processTracker.IsBlacklisted(commandLine) {
					log.Warnf("Terminating blacklisted  process PID=%d CommandLine=\"%s\"", pid, commandLine)
					if err := terminate(int(pid)); err != nil {
						log.Errorf("Failed to terminate process PID=%d: %s", pid, err)
					}
				}
			}
		}
	}
}

// hook setting flagProcTermEn variable
// it is also used to cleanup any structures needing to be cleaned
func hookProcTerm(h *HIDS, e *evtx.GoEvtxMap) {
	log.Debug("Process termination events are enabled")
	h.flagProcTermEn = true
	if guid, err := e.GetString(&pathSysmonProcessGUID); err == nil {
		// Releasing resources
		h.processTracker.Terminate(guid)
		h.memdumped.Del(guid)
	}
}

func hookSelfGUID(h *HIDS, e *evtx.GoEvtxMap) {
	if h.guid == "" {
		if e.EventID() == SysmonProcessCreate {
			// Sometimes it happens that other events are generated before process creation
			// Check parent image first because we launch whids.exe -h to test process termination
			// and we catch it up if we check image first
			if pimage, err := e.GetString(&pathSysmonParentImage); err == nil {
				if pimage == selfPath {
					if pguid, err := e.GetString(&pathSysmonParentProcessGUID); err == nil {
						h.guid = pguid
						log.Infof("Found self GUID from PGUID: %s", h.guid)
						return
					}
				}
			}
			if image, err := e.GetString(&pathSysmonImage); err == nil {
				if image == selfPath {
					if guid, err := e.GetString(&pathSysmonProcessGUID); err == nil {
						h.guid = guid
						log.Infof("Found self GUID: %s", h.guid)
						return
					}
				}
			}
		}
	}
}

func hookFileSystemAudit(h *HIDS, e *evtx.GoEvtxMap) {
	e.Set(&pathSysmonCommandLine, "?")
	e.Set(&pathSysmonProcessGUID, nullGUID)
	e.Set(&pathImageHashes, "?")
	if pid, err := e.GetInt(&pathFSAuditProcessId); err == nil {
		if pt := h.processTracker.GetByPID(pid); pt != nil {
			if pt.CommandLine != "" {
				e.Set(&pathSysmonCommandLine, pt.CommandLine)
			}
			if pt.hashes != "" {
				e.Set(&pathImageHashes, pt.hashes)
			}
			if pt.ProcessGUID != "" {
				e.Set(&pathSysmonProcessGUID, pt.ProcessGUID)
			}

			if obj, err := e.GetString(&pathFSAuditObjectName); err == nil {
				if fsutil.IsFile(obj) {
					pt.Stats.Files.LastAccessed.Add(obj)
				}
			}
		}
	}
}

func hookProcessIntegrityProcTamp(h *HIDS, e *evtx.GoEvtxMap) {
	// Default values
	e.Set(&pathProcessIntegrity, toString(-1.0))

	// Sysmon Create Process
	if e.EventID() == SysmonProcessTampering {
		if pid, err := e.GetInt(&pathSysmonProcessId); err == nil {
			// prevent stopping our own process, it may happen in some
			// cases when selfGuid is not found fast enough
			if pid != int64(os.Getpid()) {
				if kernel32.IsPIDRunning(int(pid)) {
					// we first need to wait main process thread
					mainTid := kernel32.GetFirstTidOfPid(int(pid))
					// if we found the main thread of pid
					if mainTid > 0 {
						hThread, err := kernel32.OpenThread(kernel32.THREAD_SUSPEND_RESUME, win32.FALSE, win32.DWORD(mainTid))
						if err != nil {
							log.Errorf("Cannot open main thread before checking integrity of PID=%d", pid)
						} else {
							defer kernel32.CloseHandle(hThread)
							if ok := kernel32.WaitThreadRuns(hThread, time.Millisecond*50, time.Millisecond*500); !ok {
								// We check whether the thread still exists
								checkThread, err := kernel32.OpenThread(kernel32.PROCESS_SUSPEND_RESUME, win32.FALSE, win32.DWORD(mainTid))
								if err == nil {
									log.Warnf("Timeout reached while waiting main thread of PID=%d", pid)
								}
								kernel32.CloseHandle(checkThread)
							} else {
								da := win32.DWORD(kernel32.PROCESS_VM_READ | kernel32.PROCESS_QUERY_INFORMATION)
								hProcess, err := kernel32.OpenProcess(da, win32.FALSE, win32.DWORD(pid))

								if err != nil {
									log.Errorf("Cannot open process to check integrity of PID=%d: %s", pid, err)
								} else {
									defer kernel32.CloseHandle(hProcess)
									bdiff, slen, err := kernel32.CheckProcessIntegrity(hProcess)
									if err != nil {
										log.Errorf("Cannot check integrity of PID=%d: %s", pid, err)
									} else {
										if slen != 0 {
											integrity := utils.Round(float64(bdiff)*100/float64(slen), 2)
											e.Set(&pathProcessIntegrity, toString(integrity))
										}
									}
								}
							}
						}
					}
				}
			}
		} else {
			log.Debugf("Cannot check integrity of PID=%d: process terminated", pid)
		}
	}
}

// too big to be put in hookEnrichAnySysmon
func hookEnrichServices(h *HIDS, e *evtx.GoEvtxMap) {
	// We do this only if we can cleanup resources
	eventID := e.EventID()
	if h.flagProcTermEn {
		switch eventID {
		case SysmonDriverLoad, SysmonWMIBinding, SysmonWMIConsumer, SysmonWMIFilter:
			// Nothing to do
			break
		case SysmonCreateRemoteThread, SysmonAccessProcess:
			e.Set(&pathSourceServices, "?")
			e.Set(&pathTargetServices, "?")

			sguidPath := &pathSysmonSourceProcessGUID
			tguidPath := &pathSysmonTargetProcessGUID

			if eventID == 8 {
				sguidPath = &pathSysmonCRTSourceProcessGuid
				tguidPath = &pathSysmonCRTTargetProcessGuid
			}

			if sguid, err := e.GetString(sguidPath); err == nil {
				// First try to resolve it by tracked process
				if t := h.processTracker.GetByGuid(sguid); t != nil {
					e.Set(&pathSourceServices, t.Services)
				} else {
					// If it fails we resolve the services by PID
					if spid, err := e.GetInt(&pathSysmonSourceProcessId); err == nil {
						if svcs, err := advapi32.ServiceWin32NamesByPid(uint32(spid)); err == nil {
							e.Set(&pathSourceServices, svcs)
						} else {
							log.Errorf("Failed to resolve service from PID=%d: %s", spid, err)
							e.Set(&pathSourceServices, errServiceResolution.Error())
						}
					}
				}
			}

			// First try to resolve it by tracked process
			if tguid, err := e.GetString(tguidPath); err == nil {
				if t := h.processTracker.GetByGuid(tguid); t != nil {
					e.Set(&pathTargetServices, t.Services)
				} else {
					// If it fails we resolve the services by PID
					if tpid, err := e.GetInt(&pathSysmonTargetProcessId); err == nil {
						if svcs, err := advapi32.ServiceWin32NamesByPid(uint32(tpid)); err == nil {
							e.Set(&pathTargetServices, svcs)
						} else {
							log.Errorf("Failed to resolve service from PID=%d: %s", tpid, err)
							e.Set(&pathTargetServices, errServiceResolution)
						}
					}
				}
			}
		default:
			e.Set(&pathServices, "?")
			// image, guid and pid are supposed to be available for all the remaining Sysmon logs
			if guid, err := e.GetString(&pathSysmonProcessGUID); err == nil {
				if pid, err := e.GetInt(&pathSysmonProcessId); err == nil {
					if track := h.processTracker.GetByGuid(guid); track != nil {
						if track.Services == "" {
							track.Services, err = advapi32.ServiceWin32NamesByPid(uint32(pid))
							if err != nil {
								log.Errorf("Failed to resolve service from PID=%d: %s", pid, err)
								track.Services = errServiceResolution.Error()
							}
						}
						e.Set(&pathServices, track.Services)
					} else {
						services, err := advapi32.ServiceWin32NamesByPid(uint32(pid))
						if err != nil {
							log.Errorf("Failed to resolve service from PID=%d: %s", pid, err)
							services = errServiceResolution.Error()
						}
						e.Set(&pathServices, services)
					}
				}
			}
		}
	}
}

func hookSetValueSize(h *HIDS, e *evtx.GoEvtxMap) {
	e.Set(&pathValueSize, toString(-1))
	if targetObject, err := e.GetString(&pathSysmonTargetObject); err == nil {
		size, err := advapi32.RegGetValueSizeFromString(targetObject)
		if err != nil {
			log.Errorf("Failed to get value size \"%s\": %s", targetObject, err)
		}
		e.Set(&pathValueSize, toString(size))
	}
}

// hook that replaces the destination hostname of Sysmon Network connection
// event with the one previously found in the DNS logs
/*func hookEnrichDNSSysmon(h *HIDS, e *evtx.GoEvtxMap) {
	if ip, err := e.GetString(&pathSysmonDestIP); err == nil {
		if dom, ok := dnsResolution[ip]; ok {
			e.Set(&pathSysmonDestHostname, dom)
		}
	}
}*/

func hookEnrichAnySysmon(h *HIDS, e *evtx.GoEvtxMap) {
	eventID := e.EventID()
	switch eventID {
	case SysmonProcessCreate, SysmonDriverLoad:
		// ProcessCreation is already processed in hookTrack
		// DriverLoad does not contain any GUID information
		break

	case SysmonCreateRemoteThread, SysmonAccessProcess:
		// Handling CreateRemoteThread and ProcessAccess events
		// Default Values for the fields
		e.Set(&pathSourceUser, "?")
		e.Set(&pathSourceIntegrityLevel, "?")
		e.Set(&pathTargetUser, "?")
		e.Set(&pathTargetIntegrityLevel, "?")
		e.Set(&pathTargetParentProcessGuid, "?")
		e.Set(&pathSourceHashes, "?")
		e.Set(&pathTargetHashes, "?")
		e.Set(&pathSrcProcessGeneScore, "-1")
		e.Set(&pathTgtProcessGeneScore, "-1")

		sguidPath := &pathSysmonSourceProcessGUID
		tguidPath := &pathSysmonTargetProcessGUID

		if eventID == SysmonCreateRemoteThread {
			sguidPath = &pathSysmonCRTSourceProcessGuid
			tguidPath = &pathSysmonCRTTargetProcessGuid
		}
		if sguid, err := e.GetString(sguidPath); err == nil {
			if tguid, err := e.GetString(tguidPath); err == nil {
				if strack := h.processTracker.GetByGuid(sguid); strack != nil {
					if strack.User != "" {
						e.Set(&pathSourceUser, strack.User)
					}
					if strack.IntegrityLevel != "" {
						e.Set(&pathSourceIntegrityLevel, strack.IntegrityLevel)
					}
					if strack.hashes != "" {
						e.Set(&pathSourceHashes, strack.hashes)
					}
					// Source process score
					e.Set(&pathSrcProcessGeneScore, toString(strack.GeneScore.Score))
				}
				if ttrack := h.processTracker.GetByGuid(tguid); ttrack != nil {
					if ttrack.User != "" {
						e.Set(&pathTargetUser, ttrack.User)
					}
					if ttrack.IntegrityLevel != "" {
						e.Set(&pathTargetIntegrityLevel, ttrack.IntegrityLevel)
					}
					if ttrack.ParentProcessGUID != "" {
						e.Set(&pathTargetParentProcessGuid, ttrack.ParentProcessGUID)
					}
					if ttrack.hashes != "" {
						e.Set(&pathTargetHashes, ttrack.hashes)
					}
					// Target process score
					e.Set(&pathTgtProcessGeneScore, toString(ttrack.GeneScore.Score))
				}
			}
		}

	default:

		if guid, err := e.GetString(&pathSysmonProcessGUID); err == nil {
			// Default value
			e.Set(&pathProcessGeneScore, "-1")

			if track := h.processTracker.GetByGuid(guid); track != nil {
				// if event does not have CommandLine field
				if !eventHas(e, &pathSysmonCommandLine) {
					e.Set(&pathSysmonCommandLine, "?")
					if track.CommandLine != "" {
						e.Set(&pathSysmonCommandLine, track.CommandLine)
					}
				}

				// if event does not have User field
				if !eventHas(e, &pathSysmonUser) {
					e.Set(&pathSysmonUser, "?")
					if track.User != "" {
						e.Set(&pathSysmonUser, track.User)
					}
				}

				// if event does not have IntegrityLevel field
				if !eventHas(e, &pathSysmonIntegrityLevel) {
					e.Set(&pathSysmonIntegrityLevel, "?")
					if track.IntegrityLevel != "" {
						e.Set(&pathSysmonIntegrityLevel, track.IntegrityLevel)
					}
				}

				// if event does not have CurrentDirectory field
				if !eventHas(e, &pathSysmonCurrentDirectory) {
					e.Set(&pathSysmonCurrentDirectory, "?")
					if track.CurrentDirectory != "" {
						e.Set(&pathSysmonCurrentDirectory, track.CurrentDirectory)
					}
				}

				// event never has ImageHashes field since it is not Sysmon standard
				e.Set(&pathImageHashes, "?")
				if track.hashes != "" {
					e.Set(&pathImageHashes, track.hashes)
				}

				// Signature information
				e.Set(&pathImageSigned, toString(track.Signed))
				e.Set(&pathImageSignature, track.Signature)
				e.Set(&pathImageSignatureStatus, track.SignatureStatus)

				// Overal criticality score
				e.Set(&pathProcessGeneScore, toString(track.GeneScore.Score))
			}
		}
	}
}

func hookClipboardEvents(h *HIDS, e *evtx.GoEvtxMap) {
	e.Set(&pathSysmonClipboardData, "?")
	if hashes, err := e.GetString(&pathSysmonHashes); err == nil {
		fname := fmt.Sprintf("CLIP-%s", sysmonArcFileRe.ReplaceAllString(hashes, ""))
		path := filepath.Join(h.config.Sysmon.ArchiveDirectory, fname)
		if fi, err := os.Stat(path); err == nil {
			// limit size of ClipboardData to 1 Mega
			if fi.Mode().IsRegular() && fi.Size() < utils.Mega {
				if data, err := ioutil.ReadFile(path); err == nil {
					// We try to decode utf16 content because regexp can only match utf8
					// Thus doing this is needed to apply detection rule on clipboard content
					if enc, err := utils.Utf16ToUtf8(data); err == nil {
						e.Set(&pathSysmonClipboardData, string(enc))
					} else {
						e.Set(&pathSysmonClipboardData, fmt.Sprintf("%q", data))
					}
				}
			}
		}
	}
}

//////////////////// Hooks' helpers /////////////////////

func dumpPidAndCompress(h *HIDS, pid int, guid, id string) {
	// prevent stopping ourself (><)
	if kernel32.IsPIDRunning(pid) && pid != os.Getpid() && !h.memdumped.Contains(guid) && !h.dumping.Contains(guid) {

		// To avoid dumping the same process twice, possible if two alerts
		// comes from the same GUID in a short period of time
		h.dumping.Add(guid)
		defer h.dumping.Del(guid)

		tmpDumpDir := filepath.Join(h.config.Dump.Dir, guid, id)
		os.MkdirAll(tmpDumpDir, utils.DefaultPerms)
		module, err := kernel32.GetModuleFilenameFromPID(int(pid))
		if err != nil {
			log.Errorf("Cannot get module filename for memory dump PID=%d: %s", pid, err)
		}
		dumpFilename := fmt.Sprintf("%s_%d_%d.dmp", filepath.Base(module), pid, time.Now().UnixNano())
		dumpPath := filepath.Join(tmpDumpDir, dumpFilename)
		log.Infof("Trying to dump memory of process PID=%d Image=\"%s\"", pid, module)
		//log.Infof("Mock dump: %s", dumpFilename)
		err = dbghelp.FullMemoryMiniDump(pid, dumpPath)
		if err != nil {
			log.Errorf("Failed to dump process PID=%d Image=%s: %s", pid, module, err)
		} else {
			// dump was successfull
			h.memdumped.Add(guid)
			h.compress(dumpPath)
		}
	} else {
		log.Warnf("Cannot dump process PID=%d, the process is already terminated", pid)
	}

}

func dumpFileAndCompress(h *HIDS, src, path string) error {
	var err error
	os.MkdirAll(path, utils.DefaultPerms)
	sha256, err := file.Sha256(src)
	if err != nil {
		return err
	}
	// replace : in case we are dumping an ADS
	base := strings.Replace(filepath.Base(src), ":", "_ADS_", -1)
	dst := filepath.Join(path, fmt.Sprintf("%d_%s.bin", time.Now().UnixNano(), base))
	// dump sha256 of file anyway
	ioutil.WriteFile(fmt.Sprintf("%s.sha256", dst), []byte(sha256), 0600)
	if !h.filedumped.Contains(sha256) {
		log.Debugf("Dumping file: %s->%s", src, dst)
		if err = fsutil.CopyFile(src, dst); err == nil {
			h.compress(dst)
			h.filedumped.Add(sha256)
		}
	}
	return err
}

func dumpEventAndCompress(h *HIDS, e *evtx.GoEvtxMap, guid string) (err error) {
	dumpPath := dumpPrepareDumpFilename(e, h.config.Dump.Dir, guid, "event.json")

	if !h.dumping.Contains(dumpPath) && !h.filedumped.Contains(dumpPath) {
		h.dumping.Add(dumpPath)
		defer h.dumping.Del(dumpPath)

		var f *os.File

		f, err = os.Create(dumpPath)
		if err != nil {
			return
		}
		f.Write(evtx.ToJSON(e))
		f.Close()
		h.compress(dumpPath)
		h.filedumped.Add(dumpPath)
	}
	return
}

//////////////////// Post Detection Hooks /////////////////////

// variables specific to post-detection hooks
var (
	sysmonArcFileRe = regexp.MustCompile("(((SHA1|MD5|SHA256|IMPHASH)=)|,)")
)

func dumpPrepareDumpFilename(e *evtx.GoEvtxMap, dir, guid, filename string) string {
	id := utils.HashEvent(e)
	tmpDumpDir := filepath.Join(dir, guid, id)
	os.MkdirAll(tmpDumpDir, utils.DefaultPerms)
	return filepath.Join(tmpDumpDir, filename)
}

func hookDumpProcess(h *HIDS, e *evtx.GoEvtxMap) {
	// We have to check that if we are handling one of
	// our event and we don't want to dump ourself
	if h.IsHIDSEvent(e) {
		return
	}

	// we dump only if alert is relevant
	if getCriticality(e) < h.config.Dump.Treshold {
		return
	}

	// if memory got already dumped
	if hasAction(e, ActionMemdump) {
		return
	}

	dumpProcessRtn(h, e)
}

// this hook can run async
func dumpProcessRtn(h *HIDS, e *evtx.GoEvtxMap) {
	// make it non blocking
	go func() {
		h.hookSemaphore.Acquire()
		defer h.hookSemaphore.Release()
		var guid string

		// it would be theoretically possible to dump a process
		// only from a PID (with a null GUID) but dumpPidAndCompress
		// is not designed for it.
		if guid = srcGUIDFromEvent(e); guid != nullGUID {
			// check if we should go on
			if !h.processTracker.CheckDumpCountOrInc(guid, h.config.Dump.MaxDumps, h.config.Dump.DumpUntracked) {
				log.Warnf("Not dumping, reached maximum dumps count for guid %s", guid)
				return
			}

			if pt := h.processTracker.GetByGuid(guid); pt != nil {
				// if the process track is not nil we are sure PID is set
				dumpPidAndCompress(h, int(pt.PID), guid, utils.HashEvent(e))
			}
		}
		dumpEventAndCompress(h, e, guid)
	}()
}

func hookDumpRegistry(h *HIDS, e *evtx.GoEvtxMap) {
	// We have to check that if we are handling one of
	// our event and we don't want to dump ourself
	if h.IsHIDSEvent(e) {
		return
	}

	// we dump only if alert is relevant
	if getCriticality(e) < h.config.Dump.Treshold {
		return
	}

	// if registry got already dumped
	if hasAction(e, ActionRegdump) {
		return
	}

	dumpRegistryRtn(h, e)
}

func dumpRegistryRtn(h *HIDS, e *evtx.GoEvtxMap) {
	// make it non blocking
	go func() {
		h.hookSemaphore.Acquire()
		defer h.hookSemaphore.Release()
		if guid, err := e.GetString(&pathSysmonProcessGUID); err == nil {

			// check if we should go on
			if !h.processTracker.CheckDumpCountOrInc(guid, h.config.Dump.MaxDumps, h.config.Dump.DumpUntracked) {
				log.Warnf("Not dumping, reached maximum dumps count for guid %s", guid)
				return
			}

			if targetObject, err := e.GetString(&pathSysmonTargetObject); err == nil {
				if details, err := e.GetString(&pathSysmonDetails); err == nil {
					// We dump only if Details is "Binary Data" since the other kinds can be seen in the raw event
					if details == "Binary Data" {
						dumpPath := filepath.Join(h.config.Dump.Dir, guid, utils.HashEvent(e), "reg.txt")
						key, value := filepath.Split(targetObject)
						dumpEventAndCompress(h, e, guid)
						content, err := utils.RegQuery(key, value)
						if err != nil {
							log.Errorf("Failed to run reg query: %s", err)
							content = fmt.Sprintf("Error Dumping %s: %s", targetObject, err)
						}
						err = ioutil.WriteFile(dumpPath, []byte(content), 0600)
						if err != nil {
							log.Errorf("Failed to write registry content to file: %s", err)
							return
						}
						h.compress(dumpPath)
						return
					}
					return
				}
			}
		}
		log.Errorf("Failed to dump registry from event")
	}()
}

func dumpCommandLine(h *HIDS, e *evtx.GoEvtxMap, dumpPath string) {
	if cl, err := e.GetString(&pathSysmonCommandLine); err == nil {
		if cwd, err := e.GetString(&pathSysmonCurrentDirectory); err == nil {
			if argv, err := utils.ArgvFromCommandLine(cl); err == nil {
				if len(argv) > 1 {
					for _, arg := range argv[1:] {
						if fsutil.IsFile(arg) && !utils.IsPipePath(arg) {
							if err = dumpFileAndCompress(h, arg, dumpPath); err != nil {
								log.Errorf("Error dumping file from EventID=%d \"%s\": %s", e.EventID(), arg, err)
							}
						}
						// try to dump a path relative to CWD
						relarg := filepath.Join(cwd, arg)
						if fsutil.IsFile(relarg) && !utils.IsPipePath(relarg) {
							if err = dumpFileAndCompress(h, relarg, dumpPath); err != nil {
								log.Errorf("Error dumping file from EventID=%d \"%s\": %s", e.EventID(), relarg, err)
							}
						}
					}
				}
			}
		}
	}
}

func dumpParentCommandLine(h *HIDS, e *evtx.GoEvtxMap, dumpPath string) {
	if guid, err := e.GetString(&pathSysmonProcessGUID); err == nil {
		if track := h.processTracker.GetByGuid(guid); track != nil {
			if argv, err := utils.ArgvFromCommandLine(track.ParentCommandLine); err == nil {
				if len(argv) > 1 {
					for _, arg := range argv[1:] {
						if fsutil.IsFile(arg) && !utils.IsPipePath(arg) {
							if err = dumpFileAndCompress(h, arg, dumpPath); err != nil {
								log.Errorf("Error dumping file from EventID=%d \"%s\": %s", e.EventID(), arg, err)
							}
						}
						// try to dump a path relative to parent CWD
						if track.ParentCurrentDirectory != "" {
							relarg := filepath.Join(track.ParentCurrentDirectory, arg)
							if fsutil.IsFile(relarg) && !utils.IsPipePath(relarg) {
								if err = dumpFileAndCompress(h, relarg, dumpPath); err != nil {
									log.Errorf("Error dumping file from EventID=%d \"%s\": %s", e.EventID(), relarg, err)
								}
							}
						}
					}
				}
			}
		}
	}
}

func hookDumpFiles(h *HIDS, e *evtx.GoEvtxMap) {
	// We have to check that if we are handling one of
	// our event and we don't want to dump ourself
	if h.IsHIDSEvent(e) {
		return
	}

	// we dump only if alert is relevant
	if getCriticality(e) < h.config.Dump.Treshold {
		return
	}

	// if file got already dumped
	if hasAction(e, ActionFiledump) {
		return
	}

	dumpFilesRtn(h, e)
}

func dumpFilesRtn(h *HIDS, e *evtx.GoEvtxMap) {
	// make it non blocking
	go func() {
		h.hookSemaphore.Acquire()
		defer h.hookSemaphore.Release()
		guid := srcGUIDFromEvent(e)

		// check if we should go on
		if !h.processTracker.CheckDumpCountOrInc(guid, h.config.Dump.MaxDumps, h.config.Dump.DumpUntracked) {
			log.Warnf("Not dumping, reached maximum dumps count for guid %s", guid)
			return
		}

		// build up dump path
		dumpPath := filepath.Join(h.config.Dump.Dir, guid, utils.HashEvent(e))
		// dump event who triggered the dump
		dumpEventAndCompress(h, e, guid)

		// dump CommandLine fields regardless of the event
		// this would actually work best when hooks are enabled and enrichment occurs
		// in the worst case it would only work for Sysmon CreateProcess events
		dumpCommandLine(h, e, dumpPath)
		dumpParentCommandLine(h, e, dumpPath)

		// Handling different kinds of event IDs
		switch e.EventID() {

		case SysmonFileTime, SysmonFileCreate, SysmonCreateStreamHash:
			if target, err := e.GetString(&pathSysmonTargetFilename); err == nil {
				if err = dumpFileAndCompress(h, target, dumpPath); err != nil {
					log.Errorf("Error dumping file from EventID=%d \"%s\": %s", e.EventID(), target, err)
				}
			}

		case SysmonDriverLoad:
			if im, err := e.GetString(&pathSysmonImageLoaded); err == nil {
				if err = dumpFileAndCompress(h, im, dumpPath); err != nil {
					log.Errorf("Error dumping file from EventID=%d \"%s\": %s", e.EventID(), im, err)
				}
			}

		case SysmonAccessProcess:
			if sim, err := e.GetString(&pathSysmonSourceImage); err == nil {
				if err = dumpFileAndCompress(h, sim, dumpPath); err != nil {
					log.Errorf("Error dumping file from EventID=%d \"%s\": %s", e.EventID(), sim, err)
				}
			}

		case SysmonRegSetValue, SysmonWMIConsumer:
			// for event ID 13
			path := &pathSysmonDetails
			if e.EventID() == SysmonWMIConsumer {
				path = &pathSysmonDestination
			}
			if cl, err := e.GetString(path); err == nil {
				// try to parse details as a command line
				if argv, err := utils.ArgvFromCommandLine(cl); err == nil {
					for _, arg := range argv {
						if fsutil.IsFile(arg) && !utils.IsPipePath(arg) {
							if err = dumpFileAndCompress(h, arg, dumpPath); err != nil {
								log.Errorf("Error dumping file from EventID=%d \"%s\": %s", e.EventID(), arg, err)
							}
						}
					}
				}
			}

		case SysmonFileDelete:
			if im, err := e.GetString(&pathSysmonImage); err == nil {
				if err = dumpFileAndCompress(h, im, dumpPath); err != nil {
					log.Errorf("Error dumping file from EventID=%d \"%s\": %s", e.EventID(), im, err)
				}
			}

			archived, err := e.GetBool(&pathSysmonArchived)
			if err == nil && archived {
				if !fsutil.IsDir(h.config.Sysmon.ArchiveDirectory) {
					log.Errorf("Aborting deleted file dump: %s archive directory does not exist", h.config.Sysmon.ArchiveDirectory)
					return
				}
				log.Info("Will try to dump deleted file")
				if hashes, err := e.GetString(&pathSysmonHashes); err == nil {
					if target, err := e.GetString(&pathSysmonTargetFilename); err == nil {
						fname := fmt.Sprintf("%s%s", sysmonArcFileRe.ReplaceAllString(hashes, ""), filepath.Ext(target))
						path := filepath.Join(h.config.Sysmon.ArchiveDirectory, fname)
						if err = dumpFileAndCompress(h, path, dumpPath); err != nil {
							log.Errorf("Error dumping file from EventID=%d \"%s\": %s", e.EventID(), path, err)
						}
					}
				}
			}

		default:
			if im, err := e.GetString(&pathSysmonImage); err == nil {
				if err = dumpFileAndCompress(h, im, dumpPath); err != nil {
					log.Errorf("Error dumping file from EventID=%d \"%s\": %s", e.EventID(), im, err)
				}
			}
			if pim, err := e.GetString(&pathSysmonParentImage); err == nil {
				if err = dumpFileAndCompress(h, pim, dumpPath); err != nil {
					log.Errorf("Error dumping file from EventID=%d \"%s\": %s", e.EventID(), pim, err)
				}
			}
		}
	}()
}

func hookDumpReport(h *HIDS, e *evtx.GoEvtxMap) {
	// We have to check that if we are handling one of
	// our event and we don't want to dump ourself
	if h.IsHIDSEvent(e) {
		return
	}

	// we dump only if alert is relevant
	if getCriticality(e) < h.config.Dump.Treshold {
		return
	}

	// if file got already dumped
	if hasAction(e, ActionReport) {
		return
	}

	dumpReportRtn(h, e)
}

func dumpReportRtn(h *HIDS, e *evtx.GoEvtxMap) {
	// make it non blocking
	go func() {
		h.hookSemaphore.Acquire()
		defer h.hookSemaphore.Release()

		c := h.config.Report
		guid := srcGUIDFromEvent(e)

		// check if we should go on
		if !h.processTracker.CheckDumpCountOrInc(guid, h.config.Dump.MaxDumps, h.config.Dump.DumpUntracked) {
			log.Warnf("Not dumping, reached maximum dumps count for guid %s", guid)
			return
		}
		reportPath := dumpPrepareDumpFilename(e, h.config.Dump.Dir, guid, "report.json")
		//psPath := dumpPrepareDumpFilename(e, h.config.Dump.Dir, guid, "ps.json")
		dumpEventAndCompress(h, e, guid)
		if c.EnableReporting {
			log.Infof("Generating IR report: %s", guid)
			if b, err := json.Marshal(h.Report()); err != nil {
				log.Errorf("Failed to JSON encode report: %s", guid)
			} else {
				utils.HidsWriteFile(reportPath, b)
				h.compress(reportPath)
			}
			log.Infof("Finished generating report: %s", guid)
		}

	}()
}
