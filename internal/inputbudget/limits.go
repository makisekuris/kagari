package inputbudget

import (
	"errors"
	"fmt"
)

const CountBasis = "utf8_bytes"

var ErrInvalidLimits = errors.New("inputbudget: invalid context limits")

// Limits is the per-call input allowance derived from the endpoint's total window.
type Limits struct {
	Window      int64
	Output      int64
	Reserve     int64
	Hard        int64
	SummarizeAt int64
	Target      int64
}

func NewLimits(window, output int64) (Limits, error) {
	if window <= 0 || output <= 0 || output >= window {
		return Limits{}, fmt.Errorf("%w: window and output must be positive and output must be less than window", ErrInvalidLimits)
	}
	reserve := window / 20
	if window%20 != 0 {
		reserve++
	}
	if reserve < 1024 {
		reserve = 1024
	}
	remaining := window - output
	if remaining <= reserve || remaining-reserve < 4096 {
		return Limits{}, fmt.Errorf("%w: input allowance must be at least 4096 tokens", ErrInvalidLimits)
	}
	hard := remaining - reserve
	return Limits{
		Window: window, Output: output, Reserve: reserve, Hard: hard,
		SummarizeAt: hard/5*4 + hard%5*4/5,
		Target:      hard/5*3 + hard%5*3/5,
	}, nil
}

func (l Limits) WithOutput(output int64) (Limits, error) {
	return NewLimits(l.Window, output)
}
