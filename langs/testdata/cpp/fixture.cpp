#include <string>

class Logger {
public:
    void log(const std::string& message) {
        // write message
    }
};

std::string format(const std::string& msg) {
    return "[INFO] " + msg;
}
