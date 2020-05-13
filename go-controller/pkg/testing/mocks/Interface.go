// Code generated by mockery v1.0.0. DO NOT EDIT.

package mocks

import (
	context "context"

	exec "k8s.io/utils/exec"

	mock "github.com/stretchr/testify/mock"
)

// Interface is an autogenerated mock type for the Interface type
type Interface struct {
	mock.Mock
}

// Command provides a mock function with given fields: cmd, args
func (_m *Interface) Command(cmd string, args ...string) exec.Cmd {
	_va := make([]interface{}, len(args))
	for _i := range args {
		_va[_i] = args[_i]
	}
	var _ca []interface{}
	_ca = append(_ca, cmd)
	_ca = append(_ca, _va...)
	ret := _m.Called(_ca...)

	var r0 exec.Cmd
	if rf, ok := ret.Get(0).(func(string, ...string) exec.Cmd); ok {
		r0 = rf(cmd, args...)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(exec.Cmd)
		}
	}

	return r0
}

// CommandContext provides a mock function with given fields: ctx, cmd, args
func (_m *Interface) CommandContext(ctx context.Context, cmd string, args ...string) exec.Cmd {
	_va := make([]interface{}, len(args))
	for _i := range args {
		_va[_i] = args[_i]
	}
	var _ca []interface{}
	_ca = append(_ca, ctx, cmd)
	_ca = append(_ca, _va...)
	ret := _m.Called(_ca...)

	var r0 exec.Cmd
	if rf, ok := ret.Get(0).(func(context.Context, string, ...string) exec.Cmd); ok {
		r0 = rf(ctx, cmd, args...)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(exec.Cmd)
		}
	}

	return r0
}

// LookPath provides a mock function with given fields: file
func (_m *Interface) LookPath(file string) (string, error) {
	ret := _m.Called(file)

	var r0 string
	if rf, ok := ret.Get(0).(func(string) string); ok {
		r0 = rf(file)
	} else {
		r0 = ret.Get(0).(string)
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(string) error); ok {
		r1 = rf(file)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}
