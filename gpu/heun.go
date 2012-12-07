package gpu

import (
	"code.google.com/p/nimble-cube/core"
	"code.google.com/p/nimble-cube/dump"
	"code.google.com/p/nimble-cube/nimble"
	"github.com/barnex/cuda5/safe"
	"math"
)

// Adaptive heun solver.
// TODO: now only for magnetization (because it normalizes)
// post-step hook?
type Heun struct {
	solverCommon
	dy0   [3]safe.Float32s // buffer dy/dt
	y     nimble.ChanN
	dy    nimble.RChanN
	init  bool
}

func NewHeun(y nimble.ChanN, dy_ nimble.ChanN, dt, multiplier float64) *Heun {
	core.Assert(dt > 0 && multiplier > 0)
	dy := dy_.NewReader()
	dy0 := MakeVectors(y.BufLen()) // TODO: proper len?
	var w dump.TableWriter
	if core.DEBUG {
		w = dump.NewTableWriter(core.OpenFile(core.OD+"/debug_heun.table"),
			[]string{"t", "dt", "err"}, []string{"s", "s", y.Unit()})
	}
	return &Heun{dy0: dy0, y: y, dy: dy,
		solverCommon: solverCommon{dt_si: dt, dt_mul: multiplier, Maxerr: 1e-4, Headroom: 0.75,
			debug: w, stream: stream3Create()}}
}

// Run for a duration in seconds
func (e *Heun) Advance(seconds float64) {
	nimble.RunStack()
	core.Log("GPU heun solver:", seconds, "s")
	LockCudaThread()
	stop := e.time + seconds
	for e.time < stop {
		e.Step()
	}
	nimble.DashExit()

	if core.DEBUG {
		e.debug.Flush()
	}
}

// Run for a number of steps
func (e *Heun) Steps(steps int) {
	nimble.RunStack()
	core.Log("GPU heun solver:", steps, "steps")
	LockCudaThread()
	for s := 0; s < steps; s++ {
		e.Step()
	}
	nimble.DashExit()

	if core.DEBUG {
		e.debug.Flush()
	}
}

// Take one time step
func (e *Heun) Step() {
	n := e.y.Mesh().NCell()
	str := e.stream

	// Send out initial value
	if !e.init {
		// normalize initial magnetization
		M := Device3(e.y.UnsafeData())
		NormalizeSync(M, str[0])
		e.y.WriteNext(n)
		e.init = true
	}
	e.y.WriteDone()

	dy0 := e.dy0
	dt := float32(e.dt_si * e.dt_mul) // could check here if it is in float32 ranges
	core.Assert(dt > 0)

	// stage 1
	nimble.Clock.Send(e.time, true)
	dy := Device3(e.dy.ReadNext(n))
	y := Device3(e.y.WriteNext(n))
	maddvec(y, dy, dt, str)
	e.y.WriteDone()
	cpyvec(dy0, dy, str)
	e.dy.ReadDone()

	// stage 2
	nimble.Clock.Send(e.time+e.dt_si, false)
	dy = Device3(e.dy.ReadNext(n))
	y = Device3(e.y.WriteNext(n))
	{
		err := MaxVecDiff(dy0[0], dy0[1], dy0[2], dy[0], dy[1], dy[2], str[0]) * float64(dt)
		e.sendDebugOutput(err)
		e.checkErr(err)

		if err < e.Maxerr || e.dt_si <= e.Mindt { // mindt check to avoid infinite loop
			madd2vec(y, dy, dy0, 0.5*dt, -0.5*dt, str)
			NormalizeSync(y, str[0])
			e.time += e.dt_si
			e.steps++
			e.adaptDt(math.Pow(e.Maxerr/err, 1./2.))
		} else { // undo.
			maddvec(y, dy0, -dt, str)
			e.undone++
			e.adaptDt(math.Pow(e.Maxerr/err, 1./3.))
		}
		e.updateDash(err)
	}
	e.dy.ReadDone()
	// no writeDone() here.
}
