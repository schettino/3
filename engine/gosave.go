package engine

import (
	"code.google.com/p/mx3/cuda"
	"code.google.com/p/mx3/data"
	"log"
)

// Save buffer (obtained by cuda.GetBuffer()) to file.
// buffer is automatically recycled. Underlying implementation
// is concurrent. buffer should not be used after this call.
func goSaveAndRecycle(fname string, buffer *data.Slice, t float64) {
	initQue()
	dlQue <- dlTask{fname, buffer, t}
}

// Save a copy of output to file. Underlying implementation
// is concurrent. Function returns as soon as output can be
// safely modified.
func goSaveCopy(fname string, output *data.Slice, t float64) {
	cpy := cuda.GetBuffer(output.NComp(), output.Mesh())
	data.Copy(cpy, output)
	goSaveAndRecycle(fname, cpy, t)
}

var (
	dlQue   chan dlTask       // passes download requests from goSave to runDownloader
	saveQue chan saveTask     // passes save requests from runDownloader to runSaver
	hBuf    chan *data.Slice  // pool of page-locked host buffers for save queue
	done    = make(chan bool) // marks output server is completely done after closing dlQue
)

func initQue() {
	if dlQue == nil {
		dlQue = make(chan dlTask)
		saveQue = make(chan saveTask)
		hBuf = make(chan *data.Slice, maxOutputQueLen)
		go runDownloader()
		go runSaver()
	}
}

// download task
type dlTask struct {
	fname  string
	output *data.Slice // needs to be recylced
	time   float64
}

// save task
type saveTask dlTask

// At most this many outputs can be queued for asynchronous saving to disk.
const maxOutputQueLen = 16

var nOutBuf int // number of output buffers actually in use (<= maxOutputQueLen)

// returns host buffer for storing output before being flushed to disk.
// takes one from the pool or allocates a new one when the pool is empty
// and less than maxOutputQueLen buffers already are in use.
// TODO: use same cuda.GetBuffer implementation!
func hostbuf() *data.Slice {
	select {
	case b := <-hBuf:
		cuda.Memset(b, 0, 0, 0) // not strictly needed
		return b
	default:
		if nOutBuf < maxOutputQueLen {
			nOutBuf++
			return cuda.NewUnifiedSlice(3, Mesh())
		}
	}
	panic("unreachable")
}

// continuously takes download tasks and queues corresponding save tasks.
// the downloader queue is not buffered and we want to use at most one GPU
// output buffer. Only one PCIe download at a time can proceed anyway.
func runDownloader() {
	cuda.LockThread()

	for t := range dlQue {
		h := hostbuf()
		data.Copy(h, t.output) // output is already locked
		cuda.RecycleBuffer(t.output)
		saveQue <- saveTask{t.fname, h, t.time}
	}
	close(saveQue)
}

// continuously takes save tasks and flushes them to disk.
// the save queue can accommodate many outputs (stored on host).
// the rather big queue allows big output bursts to be concurrent.
func runSaver() {
	for t := range saveQue {
		data.MustWriteFile(t.fname, t.output, t.time)
		hBuf <- t.output
	}
	done <- true
}

// finalizer function called upon program exit.
// waits until all asynchronous output has been saved.
func drainOutput() {
	if dlQue != nil {
		log.Println("flushing output")
		close(dlQue)
		<-done
	}
}

// Save once, with given file name.
func saveAs(s GPU_Getter, fname string) {
	buffer, recylce := s.GetGPU()
	if recylce {
		defer cuda.RecycleBuffer(buffer)
	}
	goSaveCopy(fname, buffer, Time)
}
