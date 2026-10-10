package explorer

type fakeNetTimeout struct{}

func (fakeNetTimeout) Error() string   { return "read tcp 127.0.0.1:34612->127.0.0.1:9300: i/o timeout" }
func (fakeNetTimeout) Timeout() bool   { return true }
func (fakeNetTimeout) Temporary() bool { return true }
