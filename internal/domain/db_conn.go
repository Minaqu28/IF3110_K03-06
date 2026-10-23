package domain

type Connection struct {
	ID      int64
	Name    string
	Host    string
	Port    int64
	DBname  string
	DBuser  string
	SSLMode string
}
