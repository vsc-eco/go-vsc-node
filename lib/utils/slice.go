package utils

import (
	"reflect"

	"golang.org/x/exp/constraints"
)

func Reduce[T any, R any](a []T, reducer func(R, T) R, initial R) R {
	res := initial
	for _, v := range a {
		res = reducer(res, v)
	}
	return res
}

type Number interface {
	constraints.Float | constraints.Integer
}

func Sum[T Number](a []T) T {
	return Reduce(a, func(acc T, v T) T {
		return acc + v
	}, 0)
}

func Map[T any, R any](a []T, mapper func(T) R) []R {
	res := make([]R, len(a))
	for i, v := range a {
		res[i] = mapper(v)
	}
	return res
}

func Remove[T any](a []T, v T) []T {
	index := IndexOf(a, v)
	if index == -1 {
		return a
	}

	return Concat(a[:index], a[index+1:])
}

func IndexOf[T any](a []T, v T) int {
	for i, val := range a {
		// stupid compiler doesn't understand T is always comparable to T
		if reflect.DeepEqual(val, v) {
			return i
		}
	}
	return -1
}

func Concat[T any](a []T, b []T) []T {
	res := make([]T, len(a)+len(b))
	copy(res, a)
	copy(res[len(a):], b)
	return res
}

// In place merge sort
func MergeSort[T constraints.Ordered](a []T) {
	l := len(a)
	if l <= 1 {
		return
	}
	m := l / 2
	left := a[:m]
	right := a[m:]
	MergeSort(left)
	MergeSort(right)
	merge(left, right, m, l)
}

// [0, 2, 4] + [1, 3, 5] => [0, 1, 2] [3, 4, 5]
func merge[T constraints.Ordered](left []T, right []T, m int, l int) {
	lp := 0
	lmax := m

	rp := 0
	rmax := l - m

	for lp < lmax && rp < rmax {
		lval := left[lp]
		rval := right[rp]

		if lval > rval {
			right[rp] = lval
			left[lp] = rval

			merge(right[:rp+1], right[rp+1:], 1, rmax)
		}

		lp += 1
	}
}
