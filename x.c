#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>

#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
#pragma comment(lib, "ws2_32.lib")
#else
#include <unistd.h>
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#endif

#define PORT 1080
#define BUFFER_SIZE 1024

void die(const char *msg) {
    perror(msg);
    exit(1);
}

int main() {
    #ifdef _WIN32
    WSADATA wsa;
    if (WSAStartup(MAKEWORD(2, 2), &wsa) != 0) {
        die("WSAStartup failed");
    }
    #endif

    int sockfd = socket(AF_INET, SOCK_STREAM, 0);
    if (sockfd < 0) die("socket failed");

    struct sockaddr_in serv_addr;
    memset(&serv_addr, 0, sizeof(serv_addr));
    serv_addr.sin_family = AF_INET;
    serv_addr.sin_addr.s_addr = INADDR_ANY;
    serv_addr.sin_port = htons(PORT);

    if (bind(sockfd, (struct sockaddr *)&serv_addr, sizeof(serv_addr)) < 0)
        die("bind failed");

    if (listen(sockfd, 5) < 0) die("listen failed");

    printf("SOCKS5 server listening on port %d\n", PORT);

    while (1) {
        struct sockaddr_in cli_addr;
        socklen_t clilen = sizeof(cli_addr);
        int newsockfd = accept(sockfd, (struct sockaddr *)&cli_addr, &clilen);
        if (newsockfd < 0) die("accept failed");

        // SOCKS5 handshake
        uint8_t buffer[BUFFER_SIZE];
        int n = recv(newsockfd, (char *)buffer, BUFFER_SIZE, 0);
        if (n < 3 || buffer[0] != 0x05) {
            close(newsockfd);
            continue;
        }

        // Send auth method (no auth)
        uint8_t response[2] = {0x05, 0x00};
        send(newsockfd, (char *)response, 2, 0);

        // Get request
        n = recv(newsockfd, (char *)buffer, BUFFER_SIZE, 0);
        if (n < 7 || buffer[0] != 0x05 || buffer[1] != 0x01) {
            close(newsockfd);
            continue;
        }

        // Send success response
        uint8_t reply[10] = {0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00};
        send(newsockfd, (char *)reply, 10, 0);

        // Simple echo server for demonstration
        while ((n = recv(newsockfd, (char *)buffer, BUFFER_SIZE, 0)) > 0) {
            send(newsockfd, (char *)buffer, n, 0);
        }

        #ifdef _WIN32
        closesocket(newsockfd);
        #else
        close(newsockfd);
        #endif
    }

    #ifdef _WIN32
    closesocket(sockfd);
    WSACleanup();
    #else
    close(sockfd);
    #endif

    return 0;
}


