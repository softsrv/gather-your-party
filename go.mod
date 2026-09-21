module gather-your-party

go 1.22.3

require (
	github.com/a-h/templ v0.2.680
	github.com/joho/godotenv v1.5.1
	github.com/softsrv/steamapi v0.0.2-0.20260921072048-2a4474d771d5
)

replace github.com/softsrv/steamapi => ../steamapi
