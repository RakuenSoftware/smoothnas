package nonraid

import "fmt"

var (
	gfExp [512]byte
	gfLog [256]byte
)

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		gfExp[i] = byte(x)
		gfLog[byte(x)] = byte(i)
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11d
		}
	}
	for i := 255; i < len(gfExp); i++ {
		gfExp[i] = gfExp[i-255]
	}
}

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[int(gfLog[a])+int(gfLog[b])]
}

func gfPow(a byte, exp int) byte {
	if exp == 0 {
		return 1
	}
	if a == 0 {
		return 0
	}
	return gfExp[(int(gfLog[a])*exp)%255]
}

func gfInv(a byte) (byte, error) {
	if a == 0 {
		return 0, fmt.Errorf("zero has no inverse")
	}
	return gfExp[255-int(gfLog[a])], nil
}

func parityCoefficient(parityIdx, dataIdx int) byte {
	if parityIdx == 0 {
		return 1
	}
	return gfPow(byte(dataIdx+1), parityIdx)
}

func invertGFMatrix(matrix [][]byte) ([][]byte, error) {
	n := len(matrix)
	if n == 0 {
		return nil, fmt.Errorf("empty matrix")
	}
	aug := make([][]byte, n)
	for r := 0; r < n; r++ {
		if len(matrix[r]) != n {
			return nil, fmt.Errorf("matrix must be square")
		}
		aug[r] = make([]byte, n*2)
		copy(aug[r], matrix[r])
		aug[r][n+r] = 1
	}

	for col := 0; col < n; col++ {
		pivot := -1
		for row := col; row < n; row++ {
			if aug[row][col] != 0 {
				pivot = row
				break
			}
		}
		if pivot == -1 {
			return nil, fmt.Errorf("singular parity matrix")
		}
		if pivot != col {
			aug[pivot], aug[col] = aug[col], aug[pivot]
		}

		inv, err := gfInv(aug[col][col])
		if err != nil {
			return nil, err
		}
		for c := 0; c < n*2; c++ {
			aug[col][c] = gfMul(aug[col][c], inv)
		}

		for row := 0; row < n; row++ {
			if row == col {
				continue
			}
			factor := aug[row][col]
			if factor == 0 {
				continue
			}
			for c := 0; c < n*2; c++ {
				aug[row][c] ^= gfMul(factor, aug[col][c])
			}
		}
	}

	out := make([][]byte, n)
	for r := 0; r < n; r++ {
		out[r] = append([]byte(nil), aug[r][n:]...)
	}
	return out, nil
}
