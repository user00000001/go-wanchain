package eeee

//go:generate gencodec -type foo -formats yaml,toml,json -out mytype_json.go

type foo struct {
	A string  `json:"aaa"`
	B string  `json:"bbb" gencodec:"required"`
	C string  `toml:"ccc" gencodec:"required"`
	D string  `yml:"ddd"`
	E string  `yml:"eee" gencodec:"required"`
	F *string `json:"fff" rlp:"nil"`
	I string  `json:"iii" rlp:"-"`
}
