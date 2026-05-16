namespace MERA.SampleApp.Services;

public interface IAuthService
{
    Task<string?> AuthenticateAsync(string username, string password);
}
