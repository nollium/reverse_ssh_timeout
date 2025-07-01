$http = [System.Net.HttpListener]::new()
$http.Prefixes.Add("http://localhost:8000/")
$http.Start()

write-host "File download server running on http://localhost:8000/"

while ($http.IsListening) {
    $context = $http.GetContext()
    $filePath = $context.Request.Url.LocalPath.TrimStart('/')
    
    try {
        $fileBytes = [System.IO.File]::ReadAllBytes($filePath)
        $context.Response.ContentType = 'application/octet-stream'
        $context.Response.OutputStream.Write($fileBytes, 0, $fileBytes.Length)
    } catch {
        $context.Response.StatusCode = 404
        $buffer = [System.Text.Encoding]::UTF8.GetBytes("File not found")
        $context.Response.OutputStream.Write($buffer, 0, $buffer.Length)
    }
    
    $context.Response.Close()
}