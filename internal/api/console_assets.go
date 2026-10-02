package api

import (
	"embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed console_assets/*
var consoleAssets embed.FS

func serveConsoleIndex(c *gin.Context) {
	content, err := consoleAssets.ReadFile("console_assets/index.html")
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	c.Header("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/html; charset=utf-8", content)
}

func serveConsoleAsset(c *gin.Context) {
	name := c.Param("name")
	contentType := ""
	switch name {
	case "app.js":
		contentType = "text/javascript; charset=utf-8"
	case "style.css":
		contentType = "text/css; charset=utf-8"
	default:
		c.Status(http.StatusNotFound)
		return
	}
	content, err := consoleAssets.ReadFile("console_assets/" + name)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, contentType, content)
}
