package com.transfer.tools.controller;

import org.springframework.stereotype.Controller;
import org.springframework.web.bind.annotation.GetMapping;

/** Serves the console entry point when a browser opens a known client route directly. */
@Controller
public class ConsolePageController {

    // Keep this allowlist explicit: missing API endpoints and assets must remain errors.
    @GetMapping({"/", "/tasks", "/create", "/storage", "/nodes", "/nodes/adjustments", "/exceptions", "/tools"})
    public String consolePage() {
        return "forward:/index.html";
    }
}
