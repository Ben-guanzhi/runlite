import com.sun.net.httpserver.HttpServer;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;

/**
 * runlite 的端到端夹具：不依赖任何外部下载（纯 JDK），但行为上模拟一个 Spring Boot 服务：
 * 打印 "Started ... in ... seconds" 就绪行、绑定端口、响应 /actuator/health、
 * 并在收到结束请求时留下优雅关闭的痕迹，用来验证 runlite 的判就绪与停干净。
 */
public class E2eServer {
    public static void main(String[] args) throws Exception {
        int port = Integer.parseInt(System.getProperty("server.port", args.length > 0 ? args[0] : "18080"));
        String profile = System.getProperty("spring.profiles.active", "default");
        long t0 = System.nanoTime();

        System.out.println("  .   ____          _            __ _ _");
        System.out.println(" /\\\\ / ___'_ __ _ _(_)_ __  __ _ \\ \\ \\ \\   :: Spring Boot :: (v3.2.0)");
        System.out.println();
        System.out.println("2026-10-22T10:00:00.000+08:00  INFO 12345 --- [           main] com.example.demo.E2eServer   : Starting E2eServer using Java 17 on runlite-fixture");
        System.out.println("2026-10-22T10:00:00.000+08:00  INFO 12345 --- [           main] com.example.demo.E2eServer   : The following 1 profile is active: \"" + profile + "\"");
        System.err.println("2026-10-22T10:00:00.100+08:00  WARN 12345 --- [           main] com.example.demo.E2eServer   : 这是一行走 stderr，用来验证 runlite 的流分离");

        HttpServer server = HttpServer.create(new InetSocketAddress(port), 0);
        server.createContext("/", ex -> {
            byte[] body = "ok".getBytes(StandardCharsets.UTF_8);
            ex.sendResponseHeaders(200, body.length);
            try (OutputStream os = ex.getResponseBody()) { os.write(body); }
        });
        server.setExecutor(null);
        server.start();

        double secs = (System.nanoTime() - t0) / 1_000_000_000.0;
        System.out.printf("2026-10-22T10:00:01.000+08:00  INFO 12345 --- [           main] o.s.b.w.embedded.tomcat.TomcatWebServer  : Tomcat started on port %d (http) with context path ''%n", port);
        System.out.printf("2026-10-22T10:00:01.100+08:00  INFO 12345 --- [           main] com.example.demo.E2eServer             : Started E2eServer in %.3f seconds (process running for %.3f)%n", secs + 1.2, secs + 1.5);
        System.out.flush();

        Runtime.getRuntime().addShutdownHook(new Thread(() -> {
            System.out.println("2026-10-22T10:00:02.000+08:00  INFO 12345 --- [ionShutdownHook] o.s.b.w.e.tomcat.GracefulShutdown        : Commencing graceful shutdown. Waiting for active requests to complete");
            System.out.flush();
            server.stop(0);
        }));

        Thread.currentThread().join();
    }
}
