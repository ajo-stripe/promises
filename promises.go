package promise

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/pkg/errors"
)

type promiseType int

const (
	simpleCall promiseType = iota
	thenCall
	allCall
	raceCall
	anyCall
)

// A Promise represents an asynchronously executing unit of work
type Promise struct {
	complete   bool
	err        error
	t          promiseType
	functionRv reflect.Value
	results    []reflect.Value
	resultType []reflect.Type
	anyErrs    []error
	// returnsError is true if the last value returns an error
	returnsError bool
	cond         sync.Cond
	counter      int64
	errCounter   int64
	noCopy
}

// Used to trigger lint rules if a promise is copied
type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

func (p *Promise) raceCall(priors []*Promise, index int) (results []reflect.Value) {
	prior := priors[index]
	prior.cond.L.Lock()
	for !prior.complete {
		prior.cond.Wait()
	}
	prior.cond.L.Unlock()
	if prior.err != nil {
		panic(errors.Wrap(prior.err, "error encountered in promise"))
	}
	remaining := atomic.AddInt64(&p.counter, -1)
	if remaining == 0 {
		return prior.results[:]
	}
	return nil
}

func (p *Promise) allCall(priors []*Promise, index int) (results []reflect.Value) {
	prior := priors[index]
	prior.cond.L.Lock()
	for !prior.complete {
		prior.cond.Wait()
	}
	prior.cond.L.Unlock()
	if prior.err != nil {
		panic(errors.Wrap(prior.err, "error encountered in promise"))
	}
	remaining := atomic.AddInt64(&p.counter, -1)
	if remaining == 0 {
		size := 0
		for i := range priors {
			size += len(priors[i].resultType)
		}
		results = make([]reflect.Value, 0, size)
		for _, completedPromise := range priors {
			results = append(results, completedPromise.results...)
		}
		return results
	}
	return nil
}

// AnyErr returns when all promises passed to Any fail
type AnyErr struct {
	// Errs contains the error of all passed promises
	Errs []error
	// LastErr contains the error of the last promise to fail.
	LastErr error
}

func (err *AnyErr) Error() string {
	return fmt.Sprintf("all %d promises failed. last err=%v", len(err.Errs), err.LastErr)
}

func (p *Promise) anyCall(priors []*Promise, index int) (results []reflect.Value) {
	prior := priors[index]
	prior.cond.L.Lock()
	for !prior.complete {
		prior.cond.Wait()
	}
	prior.cond.L.Unlock()
	if prior.err != nil {
		remaining := atomic.AddInt64(&p.errCounter, -1)
		p.anyErrs[index] = prior.err
		if remaining != 0 {
			return nil
		}
		panic(AnyErr{Errs: p.anyErrs[:], LastErr: prior.err})
	}
	remaining := atomic.AddInt64(&p.counter, -1)
	if remaining == 0 {
		return prior.results[:]
	}
	return nil
}

func empty() {}

// All returns a promise that resolves if all of the passed promises
// succeed or fails if any of the passed promises panics.
func All(promises ...*Promise) *Promise {
	if len(promises) == 0 {
		return New(empty)
	}
	p := &Promise{
		cond: sync.Cond{L: &sync.Mutex{}},
		t:    allCall,
	}

	// Extract the type
	p.resultType = []reflect.Type{}
	for _, prior := range promises {
		p.resultType = append(p.resultType, prior.resultType...)
	}

	p.counter = int64(len(promises))

	for i := range promises {
		go p.run(reflect.Value{}, nil, promises, i, nil)
	}
	return p
}

const anyErrorFormat = "promise %d has an unexpected return type, expected all promises passed to Any to return the same type"

// Race returns a promise that resolves if any of the passed promises
// succeed or fails if any of the passed promises panics.
// All of the supplied promises must be of the same type.
func Race(promises ...*Promise) *Promise {
	if len(promises) == 0 {
		return New(empty)
	}

	if len(promises) == 1 {
		return promises[0]
	}

	// Check that all the promises have the same return type
	firstResultType := promises[0].resultType
	for promiseIdx, promise := range promises[1:] {
		newResultType := promise.resultType
		if len(firstResultType) != len(newResultType) {
			panic(errors.Errorf(anyErrorFormat, promiseIdx))
		}
		for index := range firstResultType {
			if firstResultType[index] != newResultType[index] {
				panic(errors.Errorf(anyErrorFormat, promiseIdx))
			}
		}
	}

	p := &Promise{
		cond: sync.Cond{L: &sync.Mutex{}},
		t:    raceCall,
	}

	// Extract the type
	p.resultType = firstResultType[:]

	p.counter = int64(1)

	for i := range promises {
		go p.run(reflect.Value{}, nil, promises, i, nil)
	}
	return p
}

// Any returns a promise that resolves if any of the passed promises
// succeed or fails if all of the passed promises panics.
// All of the supplied promises must be of the same type.
func Any(promises ...*Promise) *Promise {
	if len(promises) == 0 {
		return New(empty)
	}

	if len(promises) == 1 {
		return promises[0]
	}

	// Check that all the promises have the same return type
	firstResultType := promises[0].resultType
	for promiseIdx, promise := range promises[1:] {
		newResultType := promise.resultType
		if len(firstResultType) != len(newResultType) {
			panic(errors.Errorf(anyErrorFormat, promiseIdx))
		}
		for index := range firstResultType {
			if firstResultType[index] != newResultType[index] {
				panic(errors.Errorf(anyErrorFormat, promiseIdx))
			}
		}
	}

	p := &Promise{
		cond:    sync.Cond{L: &sync.Mutex{}},
		t:       anyCall,
		anyErrs: make([]error, len(promises)),
	}

	// Extract the type
	p.resultType = firstResultType[:]

	p.counter = int64(1)
	p.errCounter = int64(len(promises))

	for i := range promises {
		go p.run(reflect.Value{}, nil, promises, i, nil)
	}
	return p
}

func getResultType(outFunc reflect.Type) (resultType []reflect.Type, returnsError bool) {
	resultType = make([]reflect.Type, 0, outFunc.NumOut())
	for i := 0; i < outFunc.NumOut()-1; i++ {
		resultType = append(resultType, outFunc.Out(i))
	}
	// Check the last return value for being an error
	if outFunc.NumOut() > 0 {
		// If there's 0 NumOut, then there can't be an error return.
		lastResultType := outFunc.Out(outFunc.NumOut() - 1)
		if lastResultType.Name() == "error" && lastResultType.PkgPath() == "" {
			returnsError = true
		} else {
			resultType = append(resultType, lastResultType)
		}
	}
	return
}

// New returns a promise that resolves when f completes. Any panic()
// encountered will be returned as an error from Wait()
func New(f interface{}, args ...interface{}) *Promise {
	// Extract the type
	p := &Promise{
		cond: sync.Cond{L: new(sync.Mutex)},
		t:    simpleCall,
	}

	functionRv := reflect.ValueOf(f)

	if functionRv.Kind() != reflect.Func {
		panic(errors.Errorf("expected Function, got %s", functionRv.Kind()))
	}

	reflectType := functionRv.Type()

	inputs := []reflect.Type{}
	for i := 0; i < reflectType.NumIn(); i++ {
		inputs = append(inputs, reflectType.In(i))
	}

	if len(args) != len(inputs) {
		panic(errors.Errorf("expected %d args, got %d args", len(inputs), len(args)))
	}

	p.resultType, p.returnsError = getResultType(reflectType)

	argValues := []reflect.Value{}

	for i := 0; i < len(args); i++ {
		providedArgRv := reflect.ValueOf(args[i])
		providedArgType := providedArgRv.Type()
		if providedArgType != inputs[i] {
			panic(errors.Errorf("for argument %d: expected type %s got type %s", i, inputs[i], providedArgType))
		}
		argValues = append(argValues, providedArgRv)
	}
	go p.run(functionRv, nil, nil, 0, argValues)
	return p
}

func (p *Promise) simpleCall(functionRv reflect.Value, argValues []reflect.Value) []reflect.Value {
	return functionRv.Call(argValues)
}

func (p *Promise) thenCall(prior *Promise, functionRv reflect.Value) []reflect.Value {
	prior.cond.L.Lock()
	for !prior.complete {
		prior.cond.Wait()
	}
	prior.cond.L.Unlock()
	if p.err != nil {
		panic(errors.Wrap(p.err, "error in previous promise"))
	}
	if prior.err != nil {
		panic(prior.err)
	}
	results := functionRv.Call(prior.results)
	return results
}

// Then returns a promise that begins execution when this Promise completes
func (p *Promise) Then(f interface{}) *Promise {
	// Extract the type
	next := &Promise{
		cond: sync.Cond{L: &sync.Mutex{}},
		t:    thenCall,
	}

	functionRv := reflect.ValueOf(f)

	if functionRv.Kind() != reflect.Func {
		panic(errors.Errorf("expected Function, got %v", functionRv.Kind()))
	}

	reflectType := functionRv.Type()

	inputs := []reflect.Type{}
	for i := 0; i < reflectType.NumIn(); i++ {
		inputs = append(inputs, reflectType.In(i))
	}
	outputs := []reflect.Type{}
	for i := 0; i < reflectType.NumOut(); i++ {
		outputs = append(outputs, reflectType.Out(i))
	}

	next.resultType, next.returnsError = getResultType(reflectType)

	// Check for variadic function
	if reflectType.IsVariadic() {
		// If it's variadic, adjust the inputs to match if possible
		argDiff := len(p.resultType) - len(inputs)
		switch {
		case argDiff == -1:
			// Skipping the variadic arg
			// TODO: better error message fo r variadic args
			inputs = inputs[:len(inputs)-1]
		case argDiff > 0:
			var variadic reflect.Type
			variadic, inputs = inputs[len(inputs)-1], inputs[:len(inputs)-1]
			for i := 0; i <= argDiff; i++ {
				// Hack: specialize the function to match the length of the incoming arguments
				inputs = append(inputs, variadic.Elem())
			}
		}
	}

	if len(inputs) != len(p.resultType) {
		panic(errors.Errorf("promise returns %d values, but provided function accepts %d args", len(p.resultType), len(inputs)))
	}

	for i := 0; i < len(p.resultType); i++ {
		if inputs[i] != p.resultType[i] {
			panic(errors.Errorf("for argument %d: expected type %s got type %s", i, p.resultType[i], inputs[i]))
		}
	}
	go next.run(functionRv, p, nil, 0, nil)
	return next
}

func (p *Promise) run(functionRv reflect.Value, prior *Promise, priors []*Promise, index int, args []reflect.Value) {
	// Catch panics
	defer func() {
		if r := recover(); r != nil {
			err, ok := r.(error)
			if !ok {
				err = errors.Errorf("%+v", r)
			}
			p.cond.L.Lock()
			defer p.cond.L.Unlock()
			if p.complete {
				return
			}
			p.err = err
			p.complete = true
			p.cond.Broadcast()
		}
	}()
	var results []reflect.Value
	switch p.t {
	case simpleCall:
		results = p.simpleCall(functionRv, args)
	case thenCall:
		results = p.thenCall(prior, functionRv)
	case allCall:
		results = p.allCall(priors, index)
		if results == nil {
			return
		}
	case anyCall:
		results = p.anyCall(priors, index)
		if results == nil {
			return
		}
	case raceCall:
		results = p.raceCall(priors, index)
	default:
		panic("unexpected call type")
	}
	p.cond.L.Lock()
	defer p.cond.L.Unlock()
	if p.returnsError {
		var lastResult reflect.Value
		lastResult, results = results[len(results)-1], results[:len(results)-1]
		if !lastResult.IsNil() {
			err, ok := lastResult.Interface().(error)
			if !ok {
				panic("Expected to find error")
			}
			p.err = err
		}
	}
	p.complete = true
	p.results = results
	p.cond.Broadcast()
}

func (p *Promise) getBareWaitRVs(out ...interface{}) []reflect.Value {
	outRvs := []reflect.Value{}
	if len(p.resultType) != len(out) {
		panic(errors.Errorf("Promise returns %d values, Wait was asked to set %d values", len(p.resultType), len(out)))
	}
	for i := 0; i < len(out); i++ {
		outRv := reflect.ValueOf(out[i])
		outRvs = append(outRvs, outRv)
		outType := outRv.Type()
		if outType != reflect.PtrTo(p.resultType[i]) {
			panic(errors.Errorf("for return value %d: expected pointer to %s got type %s", i, p.resultType[i], outType))
		}
	}
	return outRvs
}

func validSliceReturn(resultType []reflect.Type, args []interface{}) (elem reflect.Type, ok bool) {
	if len(resultType) == 0 {
		return nil, false
	}
	if len(args) != 1 {
		// we're only interested in single slice value
		return nil, false
	}

	// Check that there is only one result type
	resultElem := resultType[0]
	for _, result := range resultType[1:] {
		if result != resultElem {
			return nil, false
		}
	}
	arg := args[0]
	argType := reflect.TypeOf(arg)
	if argType.Kind() != reflect.Ptr {
		return nil, false
	}
	slice := argType.Elem()
	if slice.Kind() != reflect.Slice {
		return nil, false
	}
	elem = slice.Elem()
	if elem != resultElem {
		return nil, false
	}
	return elem, true
}

// Wait blocks until the promise finishes execution or panics.
// If the promise panics, wait wraps the panic and returns an error.
func (p *Promise) Wait(out ...interface{}) error {
	// Check for slice special case

	sliceReturnType, isSliceReturn := validSliceReturn(p.resultType, out)

	if !isSliceReturn {
		if len(p.resultType) != len(out) {
			panic(errors.Errorf("Promise returns %d values, Wait was asked to set %d values", len(p.resultType), len(out)))
		}
		for i := 0; i < len(out); i++ {
			outRv := reflect.ValueOf(out[i])
			outType := outRv.Type()
			if outType != reflect.PtrTo(p.resultType[i]) {
				panic(errors.Errorf("for return value %d: expected pointer to %s got type %s", i, p.resultType[i], outType))
			}
		}
	}
	p.cond.L.Lock()
	for !p.complete {
		p.cond.Wait()
	}
	p.cond.L.Unlock()

	if p.err != nil {
		return errors.Wrap(p.err, "error during promise execution")
	}

	var outRvs []reflect.Value

	if isSliceReturn {
		slicePtr := reflect.ValueOf(out[0])
		newSlice := reflect.MakeSlice(reflect.SliceOf(sliceReturnType), len(p.resultType), len(p.resultType))
		slicePtr.Elem().Set(newSlice)
		for i := 0; i < len(p.results); i++ {
			outRv := newSlice.Index(i)
			outRvs = append(outRvs, outRv)
		}
	} else {
		for i := 0; i < len(out); i++ {
			outRv := reflect.ValueOf(out[i])
			outRvs = append(outRvs, outRv.Elem())
		}
	}

	for i := 0; i < len(p.results); i++ {
		outRv := outRvs[i]
		result := p.results[i]
		outRv.Set(result)
	}
	return nil
}
