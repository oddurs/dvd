package main

import (
	"math"
	"math/rand"
	"strconv"
	"time"
)

// BounceEvent describes what happened during a physics step.
type BounceEvent struct {
	BouncedWall bool
	HitCorner   bool
	NearMiss    bool
	NearMissPx  int
	CornerName  string
}

// Physics simulates constant-velocity diagonal DVD logo movement in pixel coordinates.
type Physics struct {
	X, Y               float64
	VX, VY             float64
	MaxX, MaxY         float64
	Speed              float64 // diagonal speed magnitude in pixels/sec (each axis ~ Speed / sqrt(2))
	CornerTol          float64 // pixel tolerance for corner kiss
	ColorIdx           int
	NumColors          int
	Bounces            int
	Corners            int
	CelebrateRemaining float64 // seconds left in corner-hit celebration
	LastNote           string
	NoteRemaining      float64
	rng                *rand.Rand
}

func NewPhysics(maxX, maxY, speed float64, numColors int) *Physics {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	maxX = math.Max(1, maxX)
	maxY = math.Max(1, maxY)
	if numColors < 1 {
		numColors = 1
	}

	p := &Physics{
		MaxX:      maxX,
		MaxY:      maxY,
		Speed:     speed,
		CornerTol: computeCornerTol(maxX, maxY),
		ColorIdx:  rng.Intn(numColors),
		NumColors: numColors,
		rng:       rng,
	}
	p.Randomize()
	return p
}

// Randomize places the logo at a fresh interior position with a classic ~45-degree diagonal velocity.
func (p *Physics) Randomize() {
	p.X = (0.18 + 0.64*p.rng.Float64()) * p.MaxX
	p.Y = (0.18 + 0.64*p.rng.Float64()) * p.MaxY

	angle := math.Pi/4.0 + (p.rng.Float64()-0.5)*0.06 // ~45° ± 1.7°
	sx := 1.0
	if p.rng.Intn(2) == 0 {
		sx = -1.0
	}
	sy := 1.0
	if p.rng.Intn(2) == 0 {
		sy = -1.0
	}
	p.VX = sx * p.Speed * math.Cos(angle)
	p.VY = sy * p.Speed * math.Sin(angle)
	p.nextColor()
}

func computeCornerTol(maxX, maxY float64) float64 {
	minDim := math.Min(maxX, maxY)
	// ~3.5 physical pixels on Retina displays, scaled down on tiny grids.
	return math.Max(1.5, math.Min(4.0, minDim*0.006))
}

// UpdateBounds rescales/clamps the logo position smoothly when the window or logo resizes.
func (p *Physics) UpdateBounds(newMaxX, newMaxY float64) {
	newMaxX = math.Max(1, newMaxX)
	newMaxY = math.Max(1, newMaxY)
	if p.MaxX > 0 {
		p.X = (p.X / p.MaxX) * newMaxX
	}
	if p.MaxY > 0 {
		p.Y = (p.Y / p.MaxY) * newMaxY
	}
	p.X = clamp(p.X, 0, newMaxX)
	p.Y = clamp(p.Y, 0, newMaxY)
	p.MaxX = newMaxX
	p.MaxY = newMaxY
	p.CornerTol = computeCornerTol(newMaxX, newMaxY)
}

// SetSpeed updates velocity magnitude while preserving current direction.
func (p *Physics) SetSpeed(newSpeed float64) {
	newSpeed = clamp(newSpeed, 20, 4000)
	cur := math.Hypot(p.VX, p.VY)
	if cur > 1e-6 {
		scale := newSpeed / cur
		p.VX *= scale
		p.VY *= scale
	} else {
		p.VX = newSpeed * math.Cos(math.Pi/4)
		p.VY = newSpeed * math.Sin(math.Pi/4)
	}
	p.Speed = newSpeed
}

// Step advances the simulation by dt seconds and resolves wall/corner collisions.
func (p *Physics) Step(dt float64) BounceEvent {
	var ev BounceEvent
	if dt <= 0 {
		return ev
	}
	if dt > 0.1 {
		dt = 0.1
	}

	if p.CelebrateRemaining > 0 {
		p.CelebrateRemaining = math.Max(0, p.CelebrateRemaining-dt)
	}
	if p.NoteRemaining > 0 {
		p.NoteRemaining = math.Max(0, p.NoteRemaining-dt)
		if p.NoteRemaining == 0 {
			p.LastNote = ""
		}
	}

	nx := p.X + p.VX*dt
	ny := p.Y + p.VY*dt

	hitLeft := nx <= 0 && p.VX < 0
	hitRight := nx >= p.MaxX && p.VX > 0
	hitTop := ny <= 0 && p.VY < 0
	hitBottom := ny >= p.MaxY && p.VY > 0

	hitX := hitLeft || hitRight
	hitY := hitTop || hitBottom

	// Check if one axis hit a wall while the other axis is within CornerTol of its wall and heading into it.
	if hitX && !hitY {
		if ny <= p.CornerTol && p.VY < 0 {
			hitTop = true
			hitY = true
			ny = 0
		} else if ny >= p.MaxY-p.CornerTol && p.VY > 0 {
			hitBottom = true
			hitY = true
			ny = 1 * p.MaxY
		}
	} else if hitY && !hitX {
		if nx <= p.CornerTol && p.VX < 0 {
			hitLeft = true
			hitX = true
			nx = 0
		} else if nx >= p.MaxX-p.CornerTol && p.VX > 0 {
			hitRight = true
			hitX = true
			nx = p.MaxX
		}
	}

	// Reflect X if needed.
	if hitLeft {
		if hitY {
			nx = 0
			p.VX = -p.VX
		} else {
			nx, p.VX = reflect1D(p.X, p.VX, p.MaxX, dt)
		}
	} else if hitRight {
		if hitY {
			nx = p.MaxX
			p.VX = -p.VX
		} else {
			nx, p.VX = reflect1D(p.X, p.VX, p.MaxX, dt)
		}
	}

	// Reflect Y if needed.
	if hitTop {
		if hitX {
			ny = 0
			p.VY = -p.VY
		} else {
			ny, p.VY = reflect1D(p.Y, p.VY, p.MaxY, dt)
		}
	} else if hitBottom {
		if hitX {
			ny = p.MaxY
			p.VY = -p.VY
		} else {
			ny, p.VY = reflect1D(p.Y, p.VY, p.MaxY, dt)
		}
	}

	p.X = clamp(nx, 0, p.MaxX)
	p.Y = clamp(ny, 0, p.MaxY)

	if hitX && hitY {
		ev.BouncedWall = true
		ev.HitCorner = true
		ev.CornerName = cornerLabel(hitLeft, hitTop)
		p.Bounces++
		p.Corners++
		p.nextColor()
		p.CelebrateRemaining = 1.35
		p.LastNote = "CORNER HIT! (" + ev.CornerName + ")"
		p.NoteRemaining = 3.5
		return ev
	}

	if hitX || hitY {
		ev.BouncedWall = true
		p.Bounces++
		p.nextColor()

		// Check tantalizing near-miss (within ~28px of a corner!) for optional HUD flavor.
		nearThresh := math.Max(6.0, math.Min(28.0, math.Min(p.MaxX, p.MaxY)*0.045))
		missDist := 1e9
		var cname string
		if hitX {
			if p.Y < nearThresh {
				missDist = p.Y
				cname = cornerLabel(hitLeft, true)
			} else if p.MaxY-p.Y < nearThresh {
				missDist = p.MaxY - p.Y
				cname = cornerLabel(hitLeft, false)
			}
		} else if hitY {
			if p.X < nearThresh {
				missDist = p.X
				cname = cornerLabel(true, hitTop)
			} else if p.MaxX-p.X < nearThresh {
				missDist = p.MaxX - p.X
				cname = cornerLabel(false, hitTop)
			}
		}
		if missDist < nearThresh && p.CelebrateRemaining <= 0 {
			ev.NearMiss = true
			ev.NearMissPx = int(math.Max(1, math.Round(missDist)))
			ev.CornerName = cname
			p.LastNote = "So close! Missed " + cname + " by " + strconv.Itoa(ev.NearMissPx) + "px"
			p.NoteRemaining = 2.5
		}
	}

	return ev
}

func (p *Physics) nextColor() {
	if p.NumColors <= 1 {
		p.ColorIdx = 0
		return
	}
	offset := 1 + p.rng.Intn(p.NumColors-1)
	p.ColorIdx = (p.ColorIdx + offset) % p.NumColors
}

// ActiveDisplayColor returns the current color index and whether the white flash frame is active.
func (p *Physics) ActiveDisplayColor() (colorIdx int, flash bool) {
	if p.CelebrateRemaining <= 0 {
		return p.ColorIdx, false
	}
	phase := int((1.35 - p.CelebrateRemaining) * 18.0)
	if phase == 0 || phase == 2 {
		return p.ColorIdx, true
	}
	return (p.ColorIdx + phase) % p.NumColors, false
}

// AimForCorner subtly adjusts the current velocity angle (preserving exact position, speed,
// and general travel quadrant) using unfolded-billiard mirror targets so an upcoming bounce
// lands squarely in a corner.
func (p *Physics) AimForCorner() {
	speed := math.Hypot(p.VX, p.VY)
	if speed < 1e-6 || p.MaxX < 2 || p.MaxY < 2 {
		return
	}

	sx := 1.0
	if p.VX < 0 {
		sx = -1.0
	}
	sy := 1.0
	if p.VY < 0 {
		sy = -1.0
	}

	dX0 := p.MaxX - p.X
	if sx < 0 {
		dX0 = p.X
	}
	dY0 := p.MaxY - p.Y
	if sy < 0 {
		dY0 = p.Y
	}

	curAngle := math.Atan2(math.Abs(p.VY), math.Abs(p.VX))
	bestDiff := 1e9
	bestDX, bestDY := dX0+p.MaxX, dY0

	for kx := 0; kx <= 2; kx++ {
		for ky := 0; ky <= 2; ky++ {
			dx := dX0 + float64(kx)*p.MaxX
			dy := dY0 + float64(ky)*p.MaxY
			if dx < 20 && dy < 20 {
				continue
			}
			ang := math.Atan2(dy, dx)
			if ang < 0.38 || ang > 1.19 {
				continue
			}
			diff := math.Abs(ang - curAngle)
			if kx == 0 && ky == 0 && diff > 0.05 {
				diff += 0.08
			}
			if diff < bestDiff {
				bestDiff = diff
				bestDX, bestDY = dx, dy
			}
		}
	}

	norm := math.Hypot(bestDX, bestDY)
	if norm > 1e-6 {
		p.VX = sx * speed * (bestDX / norm)
		p.VY = sy * speed * (bestDY / norm)
	}
}

// PredictNextCorner scans forward in unfolded billiard space up to maxBounces X-wall hits
// using incremental modular stepping (zero math.Mod calls inside the loop).
func (p *Physics) PredictNextCorner(maxBounces int) (float64, string, bool) {
	vxAbs := math.Abs(p.VX)
	vyAbs := math.Abs(p.VY)
	if vxAbs < 1e-6 || vyAbs < 1e-6 || p.MaxX < 1 || p.MaxY < 1 || maxBounces <= 0 {
		return 0, "", false
	}

	var tFirstX float64
	firstRight := p.VX > 0
	if firstRight {
		tFirstX = (p.MaxX - p.X) / vxAbs
	} else {
		tFirstX = p.X / vxAbs
	}
	periodX := p.MaxX / vxAbs
	periodY := 2.0 * p.MaxY

	m := math.Mod(p.Y+p.VY*tFirstX, periodY)
	if m < 0 {
		m += periodY
	}
	stepMod := math.Mod(p.VY*periodX, periodY)
	if stepMod < 0 {
		stepMod += periodY
	}

	tol := p.CornerTol
	maxYMinusTol := p.MaxY - tol

	for k := 0; k < maxBounces; k++ {
		tx := tFirstX + float64(k)*periodX
		var yAt, vyAt float64
		if m < p.MaxY {
			yAt = m
			vyAt = p.VY
		} else {
			yAt = periodY - m
			vyAt = -p.VY
		}
		m += stepMod
		if m >= periodY {
			m -= periodY
		}

		if tx <= 1e-4 {
			continue
		}
		isLeft := firstRight && (k&1 == 1) || (!firstRight && (k&1 == 0))

		if yAt <= tol && vyAt <= 0 {
			return tx, cornerLabel(isLeft, true), true
		}
		if yAt >= maxYMinusTol && vyAt >= 0 {
			return tx, cornerLabel(isLeft, false), true
		}

		var dtSinceY float64
		var hitTop bool
		if vyAt > 0 {
			dtSinceY = yAt / vyAbs
			hitTop = true
		} else {
			dtSinceY = (p.MaxY - yAt) / vyAbs
			hitTop = false
		}
		if dtSinceY >= 0 && dtSinceY*vxAbs <= tol && tx-dtSinceY > 1e-4 {
			return tx - dtSinceY, cornerLabel(isLeft, hitTop), true
		}
	}
	return 0, "", false
}

func reflect1D(pos0, vel0, maxVal, t float64) (pos, vel float64) {
	if maxVal <= 0 {
		return 0, vel0
	}
	u := pos0 + vel0*t
	period := 2.0 * maxVal
	m := math.Mod(u, period)
	if m < 0 {
		m += period
	}
	if m < maxVal {
		return m, vel0
	}
	return period - m, -vel0
}

func cornerLabel(left, top bool) string {
	switch {
	case left && top:
		return "top-left"
	case !left && top:
		return "top-right"
	case left && !top:
		return "bottom-left"
	default:
		return "bottom-right"
	}
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
