package engine

import (
	"code.google.com/p/mx3/cuda"
	"code.google.com/p/mx3/data"
	"log"
	"runtime"
)

const VERSION = "mx3.0.8 α "

var UNAME = VERSION + runtime.GOOS + "_" + runtime.GOARCH + " " + runtime.Version() + "(" + runtime.Compiler + ")"

// User inputs
var (
	B_ext func() [3]float64 = ConstVector(0, 0, 0) // Externally applied field in T, homogeneous.
	geom  Shape             = nil                  // nil means universe
)

// Accessible quantities
var (
	M     magnetization // reduced magnetization (unit length)
	B_eff setterQuant   // effective field (T) output handle
	Table DataTable     // output handle for tabular data (average magnetization etc.)
)

// hidden quantities
var (
	globalmesh data.Mesh
	regions    Regions
	extFields  []extField
	itime      int //unique integer time stamp
)

func Mesh() *data.Mesh {
	checkMesh()
	return &globalmesh
}

// Add an additional space-dependent field to B_ext.
// The field is mask * multiplier, where mask typically contains space-dependent scaling values of the order of 1.
// multiplier can be time dependent.
// TODO: extend API (set one component, construct masks or read from file). Also for current.
func AddExtField(mask *data.Slice, multiplier func() float64) {
	m := cuda.GPUCopy(mask)
	extFields = append(extFields, extField{m, multiplier})
}

type extField struct {
	mask *data.Slice
	mul  func() float64
}

// maps quantity names to downloadable data. E.g. for rendering
var Quants = make(map[string]Getter)

func initialize() {

	// magnetization
	M.init()
	FFTM.init()
	Quants["m"] = &M
	Quants["mFFT"] = &fftmPower{} // for the web interface we display FFT amplitude

	regions.init()
	Quants["regions"] = &regions

	Table = *newTable("datatable")

	initDemag()

	initExchange()

	initDMI()

	initAnisotropy()

	// external field
	b_ext := adder(3, Mesh(), "B_ext", "T", func(dst *data.Slice) {
		bext := B_ext()
		cuda.AddConst(dst, float32(bext[2]), float32(bext[1]), float32(bext[0]))
		for _, f := range extFields {
			cuda.Madd2(dst, dst, f.mask, 1, float32(f.mul()))
		}
	})
	//Quants["B_ext"] = B_ext

	// effective field
	B_eff = setter(3, Mesh(), "B_eff", "T", func(dst *data.Slice, cansave bool) {
		B_demag.set(dst, cansave)
		B_exch.addTo(dst, cansave)
		B_dmi.addTo(dst, cansave)
		B_uni.addTo(dst, cansave)
		b_ext.addTo(dst, cansave)
	})
	Quants["B_eff"] = &B_eff

	initTorque()

	// solver
	torquebuffer := cuda.NewSlice(3, Mesh())

	torqueFn := func(cansave bool) *data.Slice {
		itime++
		Table.arm(cansave)      // if table output needed, quantities marked for update
		notifySave(&M, cansave) // saves m if needed
		notifySave(&FFTM, cansave)
		notifySave(&ExchangeMask, cansave)

		Torque.set(torquebuffer, cansave)

		Table.touch(cansave) // all needed quantities are now up-to-date, save them
		return torquebuffer
	}
	Solver = *cuda.NewHeun(M.buffer, torqueFn, cuda.Normalize, 1e-15, Gamma0, &Time)
}

//func sanitycheck() {
//	if Msat() == 0 {
//		log.Fatal("Msat should be nonzero")
//	}
//}

// Returns the mesh cell size in meters. E.g.:
// 	cellsize_x := CellSize()[X]
func CellSize() [3]float64 {
	c := Mesh().CellSize()
	return [3]float64{c[Z], c[Y], c[X]} // swaps XYZ
}

func WorldSize() [3]float64 {
	w := Mesh().WorldSize()
	return [3]float64{w[Z], w[Y], w[X]} // swaps XYZ
}

func GridSize() [3]int {
	n := Mesh().Size()
	return [3]int{n[Z], n[Y], n[X]} // swaps XYZ
}

func Nx() int { return GridSize()[X] }
func Ny() int { return GridSize()[Y] }
func Nz() int { return GridSize()[Z] }

// Set the simulation mesh to Nx x Ny x Nz cells of given size.
// Can be set only once at the beginning of the simulation.
func SetMesh(Nx, Ny, Nz int, cellSizeX, cellSizeY, cellSizeZ float64) {
	if Nx <= 1 {
		log.Fatal("mesh size X should be > 1, have: ", Nx)
	}
	globalmesh = *data.NewMesh(Nz, Ny, Nx, cellSizeZ, cellSizeY, cellSizeX)
	log.Println("set mesh:", Mesh().UserString())
	initialize()
}

// for lazy setmesh: set gridsize and cellsize in separate calls
var (
	gridsize []int
	cellsize []float64
)

func setGridSize(Nx, Ny, Nz int) {
	gridsize = []int{Nx, Ny, Nz}
	if cellsize != nil {
		SetMesh(Nx, Ny, Nz, cellsize[0], cellsize[1], cellsize[2])
	}
}

func setCellSize(cx, cy, cz float64) {
	cellsize = []float64{cx, cy, cz}
	if gridsize != nil {
		SetMesh(gridsize[0], gridsize[1], gridsize[2], cx, cy, cz)
	}
}

// check if mesh is set
func checkMesh() {
	if globalmesh.Size() == [3]int{0, 0, 0} {
		panic("need to set mesh first") //todo: fatal
	}
}

// check if m is set
func checkM() {
	checkMesh()
	if M.buffer.DevPtr(0) == nil {
		log.Fatal("need to initialize magnetization first")
	}
	if cuda.MaxVecNorm(M.buffer) == 0 {
		log.Fatal("need to initialize magnetization first")
	}
}

// Cleanly exits the simulation, assuring all output is flushed.
func Close() {
	log.Println("shutting down")
	drainOutput()
	Table.flush()
}
