module gather-your-party

go 1.22.3

require (
	github.com/a-h/templ v0.2.680
	github.com/joho/godotenv v1.5.1
)

require github.com/softsrv/steamapi v0.0.1

replace github.com/softsrv/steamapi => ../steamapi
