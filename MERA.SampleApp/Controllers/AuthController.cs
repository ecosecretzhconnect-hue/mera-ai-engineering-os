using Microsoft.AspNetCore.Mvc;

namespace MERA.SampleApp.Controllers;

[ApiController]
[Route("[controller]")]
public class AuthController : ControllerBase
{
    private readonly ILogger<AuthController> _logger;

    public AuthController(ILogger<AuthController> logger) => _logger = logger;

    [HttpPost("login")]
    public IActionResult Login([FromBody] LoginRequest req)
    {
        // TODO: validate credentials against user store
        if (string.IsNullOrWhiteSpace(req.Username) || string.IsNullOrWhiteSpace(req.Password))
            return BadRequest("Username and password are required.");

        _logger.LogInformation("Login attempt for user {Username}", req.Username);

        // Stub: return a placeholder token
        return Ok(new { token = "stub-jwt-token" });
    }

    [HttpPost("logout")]
    public IActionResult Logout()
    {
        return Ok();
    }
}

public record LoginRequest(string Username, string Password);
